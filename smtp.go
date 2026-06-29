package smtpgateway

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/stupoid/smtp-gateway/internal/postcat"
)

// --- SMTP protocol helpers ---

type smtpCmd struct {
	verb string // upper-case verb: HELO, MAIL, RCPT, DATA, etc.
	args string // everything after the verb, trimmed
}

// parseSMTPCommand splits "VERB args\r\n" into verb and args.
func parseSMTPCommand(line string) (verb, args string) {
	return splitVerb(line)
}

func splitVerb(line string) (verb, rest string) {
	line = strings.TrimSpace(line)
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return strings.ToUpper(line), ""
	}
	return strings.ToUpper(line[:i]), strings.TrimSpace(line[i:])
}

// SMTP transaction phases used for command sequencing checks.
const (
	phaseInit = iota
	phaseHelo
	phaseMail
	phaseRcpt
)

// isAllowedDuringBdat returns true when the verb is permitted during a
// BDAT chunk sequence.  Only BDAT itself, RSET, NOOP, and QUIT are allowed.
func isAllowedDuringBdat(verb string) bool {
	switch verb {
	case "BDAT", "RSET", "NOOP", "QUIT":
		return true
	}
	return false
}

// --- Connection handler ---

func (s *Server) handleConn(netConn net.Conn) {
	remote := netConn.RemoteAddr().String()
	s.logDebug("connection_opened", slog.String("remote", remote))

	conn := &connState{
		netConn: netConn,
		r:       bufio.NewReaderSize(netConn, maxLineLength),
		w:       bufio.NewWriter(netConn),
	}

	connCtx, connCancel := context.WithCancel(s.ctx)
	conn.ctx = connCtx
	defer connCancel()

	// When the connection context is cancelled (e.g. server shutdown),
	// close the underlying net.Conn after a brief grace period.  If the
	// worker is in the select loop it will notice conn.ctx.Done(), send
	// a 421, and return before the close fires.  If it is stuck inside a
	// body read (readDotUnstuffed) or TLS handshake, the close unblocks
	// the read/write immediately so Shutdown doesn't hang.
	go func() {
		<-connCtx.Done()
		time.Sleep(50 * time.Millisecond)
		_ = conn.netConn.Close()
	}()

	defer func() {
		if r := recover(); r != nil {
			s.logError("handler_panic", slog.Any("panic", r))
		}
		_ = conn.Close()
		s.logDebug("connection_closed", slog.String("remote", remote))
	}()

	// Apply read deadlines and idle timeout.
	if s.IdleTimeout > 0 {
		conn.SetReadDeadline(time.Now().Add(s.IdleTimeout))
	}

	// Send banner.
	if err := conn.write(fmt.Sprintf("220 %s ESMTP\r\n", s.Hostname), s.WriteTimeout); err != nil {
		s.logError("banner_write_error", slog.String("error", err.Error()))
		return
	}

	tx := s.newTx(netConn)

	var (
		phase    = phaseInit
		gotHelo  bool
		tlsReady bool
		inBdat   bool

		// Pipelining: reader sends commands to a channel.
		events   = make(chan smtpCmd, 32)
		resumeCh = make(chan struct{}, 1)
	)

	// Start the SMTP command reader goroutine.
	go s.readCommands(conn, events, resumeCh)

	// Worker loop: process commands sequentially.
	for {
		select {
		case cmd, ok := <-events:
			if !ok {
				return // reader closed (connection error or EOF)
			}
			var resp *Response

			// During a BDAT chunk sequence, only BDAT, RSET, NOOP,
			// and QUIT are allowed.  DATA/STARTTLS must signal
			// resumeCh since the reader is paused waiting for them.
			if inBdat && !isAllowedDuringBdat(cmd.verb) {
				if cmd.verb == "DATA" || cmd.verb == "STARTTLS" {
					resumeCh <- struct{}{}
				}
				resp = &Response{503, "5.5.1 Bad sequence of commands"}
			} else {
				switch cmd.verb {
				case "HELO":
					resp, gotHelo = s.handleHelo(conn, cmd, tx, gotHelo)
					if gotHelo {
						phase = phaseHelo
					}
				case "EHLO":
					resp, gotHelo = s.handleEhlo(conn, cmd, tx, gotHelo)
					if gotHelo {
						phase = phaseHelo
					}
				case "STARTTLS":
					resp, tlsReady = s.handleStartTLS(conn, tx, gotHelo, tlsReady, resumeCh)
					if tlsReady {
						gotHelo = false
						phase = phaseInit
					}
				case "MAIL":
					resp = s.handleMail(conn, cmd, tx, phase, gotHelo, tlsReady)
					if resp.Code == 250 {
						phase = phaseMail
					}
				case "RCPT":
					resp = s.handleRcpt(conn, cmd, tx, &phase, gotHelo)
				case "DATA":
					resp = s.handleData(conn, cmd, tx, phase, resumeCh)
					// RFC 5321 §4.1.1.4: reset mail transaction state
					// after DATA regardless of acceptance/rejection.
					// The session returns to post-HELO state: HELO is
					// still valid, but MAIL FROM / RCPT TO are cleared.
					if resp != nil {
						phase = phaseHelo
						tx = s.newTx(netConn)
						conn.bodyBuf = nil
					}
				case "BDAT":
					resp = s.handleBdat(conn, cmd, tx, phase, &inBdat, resumeCh)
					if resp != nil && resp.Code == 250 && !inBdat {
						// Transaction reset on LAST chunk (handleBdat clears inBdat).
						phase = phaseInit
						tx = s.newTx(netConn)
					}
				case "RSET":
					resp = &Response{250, "2.0.0 OK"}
					tx = s.newTx(netConn)
					conn.bodyBuf = nil
					phase = phaseInit
					inBdat = false
				case "NOOP":
					resp = &Response{250, "2.0.0 OK"}
				case "VRFY", "EXPN":
					resp = RespVrfyDisabled
				case "QUIT":
					_ = conn.write(RespGoodbye.String(), s.WriteTimeout)
					return
				default:
					resp = &Response{500, "5.5.1 Command not recognised"}
				}
			}

			if resp != nil {
				if err := conn.write(resp.String(), s.WriteTimeout); err != nil {
					s.logError("write_error", slog.String("error", err.Error()))
					return
				}
			}

		case <-conn.ctx.Done():
			// Connection closed or server is shutting down.
			_ = conn.write("421 4.3.0 Service shutting down\r\n", s.WriteTimeout)
			return
		}
	}
}

// readCommands reads SMTP commands from the connection and sends them
// to events.  During DATA, it pauses so the worker can read the body
// directly.  After receiving bodyDone, it resumes normal reading.
func (s *Server) readCommands(
	conn *connState,
	events chan<- smtpCmd,
	resumeCh <-chan struct{},
) {
	defer func() {
		if r := recover(); r != nil {
			s.logError("reader_panic", slog.Any("panic", r))
		}
		close(events)
	}()

	for {
		// Apply idle deadline if configured.
		if s.IdleTimeout > 0 {
			conn.SetReadDeadline(time.Now().Add(s.IdleTimeout))
		}

		line, err := readLine(conn.r, s.ReadTimeout, conn)
		if err != nil {
			if !errors.Is(err, io.EOF) && !isTimeout(err) {
				s.logError("read_error", slog.String("error", err.Error()))
			}
			return
		}

		// Reset idle deadline after successful read.
		if s.IdleTimeout > 0 {
			conn.SetReadDeadline(time.Now().Add(s.IdleTimeout))
		}

		verb, args := parseSMTPCommand(line)
		s.logInfo("smtp_recv",
			slog.String("verb", verb),
			slog.String("args", truncate(args, 120)),
		)

		if verb == "DATA" || verb == "BDAT" || verb == "STARTTLS" {
			// Send DATA/BDAT/STARTTLS so the worker can take over the connection
			// (body read or TLS handshake) without the reader racing for bytes.
			select {
			case events <- smtpCmd{verb: verb, args: args}:
			case <-conn.ctx.Done():
				return
			}
			// Pause until worker signals resume (body read / TLS upgrade done).
			select {
			case <-resumeCh:
			case <-conn.ctx.Done():
				return
			case <-time.After(s.ReadTimeout + time.Minute):
				s.logError("resume_timeout")
				return
			}
		} else {
			select {
			case events <- smtpCmd{verb: verb, args: args}:
			case <-conn.ctx.Done():
				return
			}
		}

		if verb == "QUIT" {
			return
		}
	}
}

// --- Data reading ---

// readDotUnstuffed reads a dot-stuffed body from r until the
// terminator "\r\n.\r\n".  Returns the unstuffed bytes (raw RFC 5322
// message).  Respects maxSize (0 = unlimited).  On overflow, drains
// the remaining body lines and returns ErrMessageTooLarge so the
// protocol stream stays synchronised.
func readDotUnstuffed(r *bufio.Reader, maxSize int, conn *connState, readTimeout time.Duration) ([]byte, error) {
	var buf []byte
	if maxSize > 0 {
		// Start small — most messages are well under the limit.
		// pre-allocating maxSize (25 MiB by default) wastes heap
		// on every DATA command, and the few reallocations during
		// append growth are cheaper than scanning 25 MiB of unused
		// capacity at GC time.
		buf = make([]byte, 0, 4096)
	}
	for {
		line, err := readLine(r, readTimeout, conn)
		if err != nil {
			return buf, fmt.Errorf("reading body: %w", err)
		}
		// Terminator: a line consisting solely of ".\r\n" → readLine
		// returns ".\r\n" stripped to "." (or just ".").
		if line == "." {
			return buf, nil
		}
		// Dot-stuffing: if the line starts with ".", remove the leading dot.
		if len(line) > 0 && line[0] == '.' {
			line = line[1:]
		}
		if maxSize > 0 && len(buf)+len(line)+2 > maxSize {
			// Drain remaining body lines so the protocol stream stays
			// synchronised.  If a read error occurs during drain
			// (the client is likely waiting for the 552 response),
			// return immediately — the overflow was already detected.
			for {
				drainLine, drainErr := readLine(r, readTimeout, conn)
				if drainErr != nil {
					return buf, ErrMessageTooLarge
				}
				if drainLine == "." {
					return buf, ErrMessageTooLarge
				}
			}
		}
		buf = append(buf, line...)
		buf = append(buf, '\r', '\n')
	}
}

// ErrMessageTooLarge is returned by readDotUnstuffed when the body
// exceeds MaxMessageSize.
var ErrMessageTooLarge = errors.New("message exceeds maximum size")

// ErrBadSyntax is returned when an SMTP command argument cannot be parsed.
var ErrBadSyntax = errors.New("bad SMTP command syntax")

// --- Command handlers ---

// validateHeloDomain checks the HELO/EHLO argument for length and
// character sanity.  RFC 5321 requires a domain name or address
// literal; we reject control characters and excessively long values
// as defence-in-depth against downstream handlers that may store or
// log the domain unsafely.
func validateHeloDomain(domain string) bool {
	if len(domain) > 255 {
		return false
	}
	for i := 0; i < len(domain); i++ {
		c := domain[i]
		if c < 0x20 || c == 0x7F {
			return false
		}
	}
	return true
}

func (s *Server) handleHelo(
	conn *connState, cmd smtpCmd, tx *Tx, gotHelo bool,
) (*Response, bool) {
	if gotHelo {
		return &Response{503, "5.5.1 HELO already received"}, gotHelo
	}
	tx.Helo = cmd.args
	if tx.Helo == "" {
		return &Response{501, "5.5.2 HELO requires domain"}, false
	}
	if !validateHeloDomain(tx.Helo) {
		return &Response{501, "5.5.2 Invalid HELO domain"}, false
	}
	resp := s.Handler.Hello(conn.ctx, tx)
	if resp == nil || resp.Code != 250 {
		if resp == nil {
			resp = RespBadSeq
		}
		return resp, false
	}
	return &Response{250, s.Hostname}, true
}

func (s *Server) handleEhlo(
	conn *connState, cmd smtpCmd, tx *Tx, gotHelo bool,
) (*Response, bool) {
	if gotHelo {
		return &Response{503, "5.5.1 EHLO already received"}, gotHelo
	}
	tx.Helo = cmd.args
	if tx.Helo == "" {
		return &Response{501, "5.5.2 EHLO requires domain"}, false
	}
	if !validateHeloDomain(tx.Helo) {
		return &Response{501, "5.5.2 Invalid EHLO domain"}, false
	}
	resp := s.Handler.Hello(conn.ctx, tx)
	if resp == nil || resp.Code != 250 {
		if resp == nil {
			resp = RespBadSeq
		}
		return resp, false
	}
	// Build multi-line EHLO response with extension list.
	var ext []string
	ext = append(ext, s.Hostname+" Hello "+tx.Helo)
	ext = append(ext, "PIPELINING")
	ext = append(ext, "8BITMIME")
	ext = append(ext, "ENHANCEDSTATUSCODES")
	ext = append(ext, "SMTPUTF8")
	ext = append(ext, "CHUNKING")
	if s.TLSConfig != nil {
		ext = append(ext, "STARTTLS")
	}
	if s.MaxMessageSize > 0 {
		ext = append(ext, fmt.Sprintf("SIZE %d", s.MaxMessageSize))
	}
	for i, line := range ext {
		var err error
		if i == len(ext)-1 {
			err = conn.write(fmt.Sprintf("250 %s\r\n", line), s.WriteTimeout)
		} else {
			err = conn.write(fmt.Sprintf("250-%s\r\n", line), s.WriteTimeout)
		}
		if err != nil {
			s.logError("ehlo_write_error", slog.String("error", err.Error()))
			return nil, false
		}
	}
	return nil, true // already wrote response
}

func (s *Server) handleStartTLS(
	conn *connState, tx *Tx,
	gotHelo, tlsReady bool,
	resumeCh chan<- struct{},
) (*Response, bool) {
	// Signal resumeCh on every return — the reader goroutine is paused
	// waiting for the TLS handshake to finish.
	defer func() { resumeCh <- struct{}{} }()

	if tlsReady {
		return &Response{503, "5.5.1 STARTTLS already done"}, true
	}
	if !gotHelo {
		return &Response{503, "5.5.1 EHLO required first"}, false
	}
	if s.TLSConfig == nil {
		return &Response{502, "5.5.1 STARTTLS not supported"}, false
	}
	if err := conn.write("220 2.0.0 Ready to start TLS\r\n", s.WriteTimeout); err != nil {
		return &Response{454, "4.7.0 TLS handshake failed"}, false
	}

	// Apply a deadline to prevent a slow TLS handshake from blocking
	// the worker goroutine indefinitely.  TLS handshake timeout is
	// independent of the SMTP read timeout — 30 seconds is ample.
	tlsTimeout := 30 * time.Second
	_ = conn.netConn.SetDeadline(time.Now().Add(tlsTimeout))
	tlsConn := tls.Server(conn.netConn, s.TLSConfig.Clone())
	if err := tlsConn.Handshake(); err != nil {
		_ = conn.netConn.SetDeadline(time.Time{})
		s.logError("tls_handshake_error", slog.String("error", err.Error()))
		_ = conn.Close()
		return nil, false
	}
	_ = conn.netConn.SetDeadline(time.Time{})
	conn.upgradeToTLS(tlsConn)

	cs := tlsConn.ConnectionState()
	tx.TLS = &cs

	return nil, true // no response line needed; client will re-EHLO
}

func (s *Server) handleMail(
	conn *connState, cmd smtpCmd, tx *Tx,
	phase int, gotHelo, tlsReady bool,
) *Response {
	if s.TLSConfig != nil && !tlsReady {
		return &Response{530, "5.7.0 STARTTLS required first"}
	}
	if !gotHelo {
		return &Response{503, "5.5.1 EHLO required first"}
	}
	if phase >= phaseMail {
		return &Response{503, "5.5.1 MAIL already sent"}
	}

	// Parse "FROM:<reverse-path> [SIZE=... BODY=...]"
	from, params, err := parseMailFrom(cmd.args)
	if err != nil {
		return &Response{501, "5.5.4 Bad MAIL FROM syntax"}
	}
	tx.MailFrom = from
	tx.Params = params
	tx.Rcpts = nil
	tx.Accepted = nil
	tx.Rejected = nil
	conn.bodyBuf = nil

	if s.MaxMessageSize > 0 {
		if sizeStr, ok := params["SIZE"]; ok {
			size, err := strconv.ParseInt(sizeStr, 10, 64)
			if err != nil || size < 0 || size > int64(s.MaxMessageSize) {
				if err != nil {
					return &Response{501, "5.5.4 Bad SIZE parameter"}
				}
				return RespMessageSize
			}
		}
	}
	// MaxRecipients is enforced at the RCPT phase.

	resp := s.Handler.MailFrom(conn.ctx, tx)
	if resp == nil {
		resp = RespBadSeq
	}
	return resp
}

func (s *Server) handleRcpt(
	conn *connState, cmd smtpCmd, tx *Tx,
	phase *int, gotHelo bool,
) *Response {
	if !gotHelo {
		return &Response{503, "5.5.1 EHLO required first"}
	}
	if *phase < phaseMail {
		return &Response{503, "5.5.1 MAIL required first"}
	}
	rcpt := parseRcptTo(cmd.args)
	if len(rcpt) > 320 || containsControl(rcpt) {
		return &Response{501, "5.5.2 Invalid recipient address"}
	}
	tx.Rcpts = append(tx.Rcpts, rcpt)

	if s.MaxRecipients > 0 && len(tx.Rcpts) > s.MaxRecipients {
		return &Response{452, "4.5.3 Too many recipients"}
	}

	// Advance phase only after validation passes — a syntax error
	// must leave the transaction state unchanged.
	*phase = phaseRcpt
	resp := s.Handler.RcptTo(conn.ctx, tx)
	if resp != nil && resp.Code == 250 {
		tx.Accepted = append(tx.Accepted, rcpt)
	} else {
		if resp == nil {
			resp = RespBadSeq
		}
		tx.Rejected = append(tx.Rejected, Rejection{
			Recipient: rcpt,
			Response:  resp,
		})
	}
	return resp
}

func (s *Server) handleData(
	conn *connState, _ smtpCmd, tx *Tx,
	phase int, resumeCh chan<- struct{},
) *Response {
	// Signal resumeCh on every return — the reader goroutine is paused
	// waiting for DATA body reading to finish.
	defer func() { resumeCh <- struct{}{} }()

	if phase < phaseRcpt {
		return &Response{503, "5.5.1 RCPT required first"}
	}
	if len(tx.Accepted) == 0 {
		return &Response{554, "5.5.1 No valid recipients"}
	}
	if err := conn.write(RespStartMail.String(), s.WriteTimeout); err != nil {
		return &Response{451, "4.3.0 System error"}
	}
	readTimeout := s.ReadTimeout

	body, err := readDotUnstuffed(
		conn.r, s.MaxMessageSize,
		conn, readTimeout,
	)
	if err != nil {
		if errors.Is(err, ErrMessageTooLarge) {
			return RespMessageSize
		}
		s.logError("data_read_error", slog.String("error", err.Error()))
		return &Response{451, "4.3.0 Error reading message"}
	}

	// 8BITMIME check: reject 8-bit content per RFC 6152 unless the
	// client declared BODY=8BITMIME or SMTPUTF8 (RFC 6531 §3.4).
	_, smtputf8 := tx.Params["SMTPUTF8"]
	if !strings.EqualFold(tx.Params["BODY"], "8BITMIME") && !smtputf8 && contains8Bit(body) {
		s.logInfo("8bit_rejected",
			slog.String("mail_from", tx.MailFrom),
			slog.Int("bytes", len(body)),
		)
		return RespEightBit
	}

	s.logInfo("data_received",
		slog.Int("bytes", len(body)),
		slog.String("mail_from", tx.MailFrom),
		slog.Int("recipients", len(tx.Accepted)),
	)

	resp := s.Handler.Data(conn.ctx, tx, body)
	if resp == nil {
		resp = RespBadSeq
	}

	if resp.Code == 250 {
		s.writePostcat(tx.MailFrom, tx.Accepted, body)
	}

	return resp
}

// --- BDAT / CHUNKING (RFC 3030) ---

// parseBdatArgs parses "BDAT" arguments: "<size> [LAST]".
func parseBdatArgs(args string) (size int, last bool, err error) {
	fields := strings.Fields(args)
	if len(fields) < 1 || len(fields) > 2 {
		return 0, false, ErrBadSyntax
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil || n < 0 {
		return 0, false, ErrBadSyntax
	}
	if len(fields) == 2 {
		if strings.ToUpper(fields[1]) != "LAST" {
			return 0, false, ErrBadSyntax
		}
		last = true
	}
	return n, last, nil
}

// readNBytes reads exactly n raw bytes from r.  No dot-unstuffing,
// no line-oriented processing — just raw binary data for BDAT chunks.
func readNBytes(r *bufio.Reader, n int, conn *connState, readTimeout time.Duration) ([]byte, error) {
	if n == 0 {
		return nil, nil
	}
	// Hard cap on a single BDAT chunk.  The MaxMessageSize check in
	// handleBdat limits total accumulated body size, but a single
	// BDAT <huge> LAST declaration could still trigger a proportional
	// make([]byte, n) before the limit check fires.  64 MiB is well
	// above the default 25 MiB MaxMessageSize while preventing OOM.
	const maxChunk = 64 << 20
	if n > maxChunk {
		return nil, fmt.Errorf("chunk size %d exceeds maximum %d", n, maxChunk)
	}
	buf := make([]byte, n)
	if readTimeout > 0 {
		conn.SetReadDeadline(time.Now().Add(readTimeout))
	}
	if _, err := io.ReadFull(r, buf); err != nil {
		return buf, err
	}
	return buf, nil
}

func (s *Server) handleBdat(
	conn *connState, cmd smtpCmd, tx *Tx,
	phase int, inBdat *bool, resumeCh chan<- struct{},
) *Response {
	// Signal resumeCh on every return — the reader goroutine is paused
	// waiting for BDAT processing to finish.
	defer func() { resumeCh <- struct{}{} }()

	// Apply a read deadline for chunk reads so a slow client doesn't
	// block the worker goroutine indefinitely.
	readTimeout := s.ReadTimeout

	var size int
	var last bool
	// discardChunk drains `size` raw bytes from the connection on rejection
	// and resets BDAT state so the connection can continue with a new
	// transaction. Uses bufio.Reader.Discard so the buffer is fixed-size regardless of
	// the declared chunk size — a client that sends BDAT <huge> before HELO
	// must not trigger a proportional allocation.
	discardChunk := func() {
		*inBdat = false
		conn.bodyBuf = nil
		if size > 0 {
			if readTimeout > 0 {
				conn.SetReadDeadline(time.Now().Add(readTimeout))
			}
			if _, discErr := conn.r.Discard(size); discErr != nil {
				s.logError("bdat_read_error", slog.String("error", discErr.Error()))
			}
		}
	}

	// Parse BDAT arguments BEFORE guard checks so we know how many
	// bytes to discard on early rejection (keeps protocol synchronised).
	var err error
	size, last, err = parseBdatArgs(cmd.args)
	if err != nil {
		discardChunk()
		return &Response{501, "5.5.4 Bad BDAT syntax"}
	}

	if phase < phaseRcpt {
		discardChunk()
		return &Response{503, "5.5.1 RCPT required first"}
	}
	if len(tx.Accepted) == 0 {
		discardChunk()
		return &Response{554, "5.5.1 No valid recipients"}
	}

	// MaxMessageSize check before reading.
	if s.MaxMessageSize > 0 && len(conn.bodyBuf)+size > s.MaxMessageSize {
		discardChunk()
		return RespMessageSize
	}

	// Read the chunk data.
	chunk, err := readNBytes(conn.r, size, conn, readTimeout)
	if err != nil {
		s.logError("bdat_read_error", slog.String("error", err.Error()))
		*inBdat = false
		conn.bodyBuf = nil
		return &Response{451, "4.3.0 Error reading chunk"}
	}

	conn.bodyBuf = append(conn.bodyBuf, chunk...)

	if !last {
		// Intermediate chunk — stay in BDAT mode.
		*inBdat = true
		return &Response{250, "2.0.0 OK"}
	}

	// LAST chunk — message complete.  RFC 3030 Section 3: CHUNKING implies
	// BINARYMIME, so no 8BITMIME check needed (unlike DATA).
	*inBdat = false

	s.logInfo("bdat_received",
		slog.Int("bytes", len(conn.bodyBuf)),
		slog.String("mail_from", tx.MailFrom),
		slog.Int("recipients", len(tx.Accepted)),
	)

	resp := s.Handler.Data(conn.ctx, tx, conn.bodyBuf)
	if resp == nil {
		resp = RespBadSeq
	}

	if resp.Code == 250 {
		s.writePostcat(tx.MailFrom, tx.Accepted, conn.bodyBuf)
	}

	conn.bodyBuf = nil
	return resp
}

// --- Address parsing ---

// parseMailFrom extracts the reverse-path and optional parameters from
// a MAIL FROM argument string.  Returns ErrBadSyntax if the argument
// cannot be parsed.
func parseMailFrom(args string) (string, map[string]string, error) {
	params := map[string]string{}
	rest := strings.TrimSpace(args)

	// Expect "FROM:<...>" or "FROM:<>"
	if len(rest) < 5 || !strings.EqualFold(rest[:5], "FROM:") {
		return "", params, ErrBadSyntax
	}
	rest = rest[5:] // skip "FROM:"
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", params, ErrBadSyntax
	}
	if rest[0] != '<' {
		return "", params, ErrBadSyntax
	}
	closingBracket := strings.IndexByte(rest, '>')
	if closingBracket < 0 {
		return "", params, ErrBadSyntax
	}
	mailFrom := rest[1:closingBracket]
	if len(mailFrom) > 320 || containsControl(mailFrom) {
		return "", params, ErrBadSyntax
	}
	rest = strings.TrimSpace(rest[closingBracket+1:])

	// Parse remaining key=value parameters and bare keywords (e.g. SMTPUTF8).
	for _, part := range strings.Fields(rest) {
		k, v, ok := strings.Cut(part, "=")
		if ok {
			params[strings.ToUpper(k)] = v
		} else {
			params[strings.ToUpper(part)] = ""
		}
	}
	return mailFrom, params, nil
}

// parseRcptTo extracts the forward-path from a RCPT TO argument string.
func parseRcptTo(args string) string {
	rest := strings.TrimSpace(args)
	if len(rest) < 3 || !strings.EqualFold(rest[:3], "TO:") {
		return rest // best-effort: return everything
	}
	rest = rest[3:]
	rest = strings.TrimSpace(rest)
	if len(rest) > 0 && rest[0] == '<' {
		rest = rest[1:]
		if closingBracket := strings.IndexByte(rest, '>'); closingBracket >= 0 {
			return rest[:closingBracket]
		}
	}
	return rest
}

// --- Connection helpers ---

// connState bundles a network connection with its buffered I/O.
type connState struct {
	netConn net.Conn
	r       *bufio.Reader
	w       *bufio.Writer
	mu      sync.Mutex      // guards writes
	bodyBuf []byte          // BDAT chunk accumulation
	ctx     context.Context // per-connection — cancelled on close or shutdown
}

func (c *connState) upgradeToTLS(tlsConn net.Conn) {
	c.netConn = tlsConn
	c.r = bufio.NewReader(tlsConn)
	c.w = bufio.NewWriter(tlsConn)
}

func (c *connState) SetReadDeadline(t time.Time) {
	_ = c.netConn.SetReadDeadline(t)
}

func (c *connState) write(s string, timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if timeout > 0 {
		if err := c.netConn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
	}
	if _, err := c.w.WriteString(s); err != nil {
		return err
	}
	if err := c.w.Flush(); err != nil {
		// Flush failed — buffered data remains.  Reset the writer so
		// the next write doesn't prepend stale data to a new response.
		c.w.Reset(c.netConn)
		return err
	}
	if timeout > 0 {
		if err := c.netConn.SetWriteDeadline(time.Time{}); err != nil {
			return err
		}
	}
	return nil
}

func (c *connState) Close() error {
	return c.netConn.Close()
}

// maxLineLength is the maximum SMTP command line length.  Lines exceeding
// this are rejected to prevent unbounded buffer growth in bufio.Reader.
const maxLineLength = 65536

// readLine reads a line from r, handling both \r\n and bare \n.
// Returns the line without the trailing newline sequence.
// If readTimeout > 0, sets a per-read deadline before calling ReadString.
func readLine(r *bufio.Reader, readTimeout time.Duration, conn *connState) (string, error) {
	if readTimeout > 0 {
		conn.SetReadDeadline(time.Now().Add(readTimeout))
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	if len(line) > maxLineLength {
		return "", errors.New("line too long")
	}
	line = strings.TrimSuffix(line, "\r\n")
	line = strings.TrimSuffix(line, "\n")
	return line, nil
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// truncate strips control characters (except tab) and truncates to n rune-safe
// bytes.  Used to sanitise SMTP command arguments for logging.
func truncate(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' || r == 0x7F {
			return -1
		}
		return r
	}, s)
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

// containsControl reports whether s contains any ASCII control character
// (0x00-0x1F excluding tab, or 0x7F DEL).
func containsControl(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 && c != '\t' || c == 0x7F {
			return true
		}
	}
	return false
}

// contains8Bit reports whether b contains any byte with the high bit set.
func contains8Bit(b []byte) bool {
	for _, c := range b {
		if c > 127 {
			return true
		}
	}
	return false
}

// writePostcat writes a postcat file if PostcatDir is configured.
// The write is best-effort — errors are logged, not returned.
func (s *Server) writePostcat(mailFrom string, accepted []string, body []byte) {
	if s.PostcatDir == "" {
		return
	}
	if path, err := postcat.Write(s.PostcatDir, mailFrom, accepted, body); err != nil {
		s.logError("postcat_write_error",
			slog.String("error", err.Error()),
			slog.String("path", path),
		)
	}
}

// newTx creates a fresh transaction state for a new connection.
func (s *Server) newTx(conn net.Conn) *Tx {
	return &Tx{
		RemoteAddr: conn.RemoteAddr(),
		Hostname:   s.Hostname,
		Params:     map[string]string{},
	}
}
