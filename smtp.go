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

	"github.com/stupoid/smtp-gateway/internal/postcat"
)

// --- SMTP protocol helpers ---

// smtpCmd represents a single parsed SMTP command.
type smtpCmd struct {
	verb string // upper-case verb: HELO, MAIL, RCPT, DATA, etc.
	args string // everything after the verb, trimmed
}

// parseSMTPCommand splits "VERB args\r\n" into verb and args.
func parseSMTPCommand(line string) (verb, args string) {
	verb, args = splitVerb(line)
	return verb, strings.TrimSpace(args)
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

// --- Connection handler ---

func (s *Server) handleConn(netConn net.Conn) {
	remote := netConn.RemoteAddr().String()
	s.logDebug("connection_opened", slog.String("remote", remote))

	conn := &connState{
		netConn: netConn,
		r:       bufio.NewReader(netConn),
		w:       bufio.NewWriter(netConn),
	}

	defer func() {
		_ = conn.Close()
		s.logDebug("connection_closed", slog.String("remote", remote))
	}()

	// Apply read deadlines and idle timeout.
	if s.IdleTimeout > 0 {
		conn.SetReadDeadline(time.Now().Add(s.IdleTimeout))
	}

	// Send banner.
	conn.write(fmt.Sprintf("220 %s ESMTP\r\n", s.Hostname), s.WriteTimeout)

	tx := s.newTx(netConn)

	var (
		phase    = phaseInit
		gotHelo  bool
		tlsReady bool
		inBdat   bool

		// Pipelining: reader sends commands to a channel.
		events   = make(chan smtpCmd, 32)
		resumeCh = make(chan struct{}, 1)

		readerDone = make(chan struct{})
	)

	// Start the SMTP command reader goroutine.
	go s.readCommands(context.Background(), conn, events, resumeCh, readerDone)

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
			if inBdat && cmd.verb != "BDAT" && cmd.verb != "RSET" && cmd.verb != "NOOP" && cmd.verb != "QUIT" {
				if cmd.verb == "DATA" || cmd.verb == "STARTTLS" {
					resumeCh <- struct{}{}
				}
				resp = &Response{503, "5.5.1 Bad sequence of commands"}
			} else {
				switch cmd.verb {
				case "HELO":
					resp, gotHelo = s.handleHelo(conn, cmd, tx, phase, gotHelo)
					if gotHelo {
						phase = phaseHelo
					}
				case "EHLO":
					resp, gotHelo = s.handleEhlo(conn, cmd, tx, phase, gotHelo)
					if gotHelo {
						phase = phaseHelo
					}
				case "STARTTLS":
					resp, tlsReady = s.handleStartTLS(conn, tx, gotHelo, tlsReady, resumeCh)
				case "MAIL":
					resp = s.handleMail(conn, cmd, tx, phase, gotHelo, tlsReady)
					if resp.Code == 250 {
						phase = phaseMail
					}
				case "RCPT":
					resp = s.handleRcpt(conn, cmd, tx, &phase, gotHelo)
					if phase < phaseRcpt && resp.Code == 250 {
						phase = phaseRcpt
					}
				case "DATA":
					resp = s.handleData(conn, cmd, tx, phase, resumeCh)
					if resp.Code == 250 {
						phase = phaseInit
						tx = s.newTx(netConn)
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
					phase = phaseInit
					inBdat = false
				case "NOOP":
					resp = &Response{250, "2.0.0 OK"}
				case "VRFY", "EXPN":
					resp = RespVrfyDisabled
				case "QUIT":
					conn.write(RespGoodbye.String(), s.WriteTimeout)
					return
				default:
					resp = &Response{500, "5.5.1 Command not recognized"}
				}
			}

			if resp != nil {
				conn.write(resp.String(), s.WriteTimeout)
			}

		case <-s.ctx.Done():
			// Server is shutting down; send 421 and close.
			conn.write("421 4.3.0 Service shutting down\r\n", s.WriteTimeout)
			return
		}
	}
}

// readCommands reads SMTP commands from the connection and sends them
// to events.  During DATA, it pauses so the worker can read the body
// directly.  After receiving bodyDone, it resumes normal reading.
func (s *Server) readCommands(
	ctx context.Context,
	conn *connState,
	events chan<- smtpCmd,
	resumeCh <-chan struct{},
	done chan<- struct{},
) {
	defer func() {
		close(done)
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
			case <-ctx.Done():
				return
			}
			// Pause until worker signals resume (body read / TLS upgrade done).
			select {
			case <-resumeCh:
			case <-ctx.Done():
				return
			}
		} else {
			select {
			case events <- smtpCmd{verb: verb, args: args}:
			case <-ctx.Done():
				return
			}
		}

		// QUIT terminates the reader.
		if verb == "QUIT" {
			return
		}
	}
}

// --- Data reading ---

// readDotUnstuffed reads a dot-stuffed body from r until the
// terminator "\r\n.\r\n".  Returns the unstuffed bytes (raw RFC 5322
// message).  Respects maxSize (0 = unlimited).  On overflow, returns
// an error and the bytes read so far.
func readDotUnstuffed(r *bufio.Reader, maxSize int, conn *connState, readTimeout time.Duration) ([]byte, error) {
	var buf []byte
	if maxSize > 0 {
		buf = make([]byte, 0, maxSize)
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
			return buf, ErrMessageTooLarge
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

func (s *Server) handleHelo(
	_ *connState, cmd smtpCmd, tx *Tx,
	phase int, gotHelo bool,
) (*Response, bool) {
	_ = phase
	if gotHelo {
		return &Response{503, "5.5.1 HELO already received"}, gotHelo
	}
	tx.Helo = cmd.args
	if tx.Helo == "" {
		return &Response{501, "5.5.2 HELO requires domain"}, false
	}
	resp := s.Handler.Hello(context.Background(), tx)
	if resp == nil || resp.Code != 250 {
		if resp == nil {
			resp = RespBadSeq
		}
		return resp, false
	}
	return &Response{250, s.Hostname}, true
}

func (s *Server) handleEhlo(
	conn *connState, cmd smtpCmd, tx *Tx,
	phase int, gotHelo bool,
) (*Response, bool) {
	_ = phase
	if gotHelo {
		return &Response{503, "5.5.1 EHLO already received"}, gotHelo
	}
	tx.Helo = cmd.args
	if tx.Helo == "" {
		return &Response{501, "5.5.2 EHLO requires domain"}, false
	}
	resp := s.Handler.Hello(context.Background(), tx)
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
	if len(ext) == 1 {
		conn.write(fmt.Sprintf("250 %s\r\n", ext[0]), s.WriteTimeout)
	} else {
		for i, line := range ext {
			if i == len(ext)-1 {
				conn.write(fmt.Sprintf("250 %s\r\n", line), s.WriteTimeout)
			} else {
				conn.write(fmt.Sprintf("250-%s\r\n", line), s.WriteTimeout)
			}
		}
	}
	return nil, true // already wrote response
}

func (s *Server) handleStartTLS(
	conn *connState, tx *Tx,
	gotHelo, tlsReady bool,
	resumeCh chan<- struct{},
) (*Response, bool) {
	if tlsReady {
		resumeCh <- struct{}{}
		return &Response{503, "5.5.1 STARTTLS already done"}, true
	}
	if !gotHelo {
		resumeCh <- struct{}{}
		return &Response{503, "5.5.1 EHLO required first"}, false
	}
	if s.TLSConfig == nil {
		resumeCh <- struct{}{}
		return &Response{502, "5.5.1 STARTTLS not supported"}, false
	}
	conn.write("220 2.0.0 Ready to start TLS\r\n", s.WriteTimeout)

	tlsConn := tls.Server(conn.netConn, s.TLSConfig.Clone())
	if err := tlsConn.Handshake(); err != nil {
		resumeCh <- struct{}{}
		s.logError("tls_handshake_error", slog.String("error", err.Error()))
		return &Response{454, "4.7.0 TLS handshake failed"}, false
	}
	conn.netConn = tlsConn
	conn.r = bufio.NewReader(tlsConn)
	conn.w = bufio.NewWriter(tlsConn)

	// Signal the reader goroutine to resume on the new TLS connection.
	resumeCh <- struct{}{}

	cs := tlsConn.ConnectionState()
	tx.TLS = &cs

	return nil, true // no response line needed; client will re-EHLO
}

func (s *Server) handleMail(
	_ *connState, cmd smtpCmd, tx *Tx,
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

	if s.MaxMessageSize > 0 {
		if sizeStr, ok := params["SIZE"]; ok {
			var size int
			if _, err := fmt.Sscanf(sizeStr, "%d", &size); err == nil && size > s.MaxMessageSize {
				return RespMessageSize
			}
		}
	}
	// MaxRecipients is enforced at the RCPT phase.

	return s.Handler.MailFrom(context.Background(), tx)
}

func (s *Server) handleRcpt(
	_ *connState, cmd smtpCmd, tx *Tx,
	phase *int, gotHelo bool,
) *Response {
	if !gotHelo {
		return &Response{503, "5.5.1 EHLO required first"}
	}
	if *phase < phaseMail {
		return &Response{503, "5.5.1 MAIL required first"}
	}
	*phase = phaseRcpt

	rcpt := parseRcptTo(cmd.args)
	tx.Rcpts = append(tx.Rcpts, rcpt)

	if s.MaxRecipients > 0 && len(tx.Rcpts) > s.MaxRecipients {
		return &Response{452, "4.5.3 Too many recipients"}
	}

	resp := s.Handler.RcptTo(context.Background(), tx)
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
	if phase < phaseRcpt {
		resumeCh <- struct{}{}
		return &Response{503, "5.5.1 RCPT required first"}
	}
	if len(tx.Accepted) == 0 {
		// No accepted recipients — skip the handler entirely.
		resumeCh <- struct{}{}
		return &Response{554, "5.5.1 No valid recipients"}
	}

	// Send 354 and read the body.
	conn.write(RespStartMail.String(), s.WriteTimeout)

	body, err := readDotUnstuffed(
		conn.r, s.MaxMessageSize,
		conn, s.ReadTimeout,
	)
	if err != nil {
		resumeCh <- struct{}{}
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
		resumeCh <- struct{}{}
		s.logInfo("8bit_rejected",
			slog.String("mail_from", tx.MailFrom),
			slog.Int("bytes", len(body)),
		)
		return RespEightBit
	}

	// Signal the reader goroutine to resume.
	resumeCh <- struct{}{}

	s.logInfo("data_received",
		slog.Int("bytes", len(body)),
		slog.String("mail_from", tx.MailFrom),
		slog.Int("recipients", len(tx.Accepted)),
	)

	resp := s.Handler.Data(context.Background(), tx, body)

	// Write postcat file if configured and accepted.
	if resp != nil && resp.Code == 250 && s.PostcatDir != "" {
		if path, err := postcat.Write(s.PostcatDir, tx.MailFrom, tx.Accepted, body); err != nil {
			s.logError("postcat_write_error",
				slog.String("error", err.Error()),
				slog.String("path", path),
			)
		}
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
	// Every return path must signal resumeCh — the reader is paused.
	if phase < phaseRcpt {
		*inBdat = false
		resumeCh <- struct{}{}
		return &Response{503, "5.5.1 RCPT required first"}
	}
	if len(tx.Accepted) == 0 {
		*inBdat = false
		resumeCh <- struct{}{}
		return &Response{554, "5.5.1 No valid recipients"}
	}

	size, last, err := parseBdatArgs(cmd.args)
	if err != nil {
		*inBdat = false
		resumeCh <- struct{}{}
		return &Response{501, "5.5.4 Bad BDAT syntax"}
	}

	// MaxMessageSize check before reading.
	if s.MaxMessageSize > 0 && len(tx.BodyBuf)+size > s.MaxMessageSize {
		// Read and discard chunk data to keep the protocol stream synchronized.
		if size > 0 {
			if _, err := readNBytes(conn.r, size, conn, s.ReadTimeout); err != nil {
				s.logError("bdat_read_error", slog.String("error", err.Error()))
				*inBdat = false
				resumeCh <- struct{}{}
				return &Response{451, "4.3.0 Error reading chunk"}
			}
		}
		*inBdat = false
		resumeCh <- struct{}{}
		return RespMessageSize
	}

	// Read the chunk data.
	chunk, err := readNBytes(conn.r, size, conn, s.ReadTimeout)
	if err != nil {
		s.logError("bdat_read_error", slog.String("error", err.Error()))
		*inBdat = false
		resumeCh <- struct{}{}
		return &Response{451, "4.3.0 Error reading chunk"}
	}

	tx.BodyBuf = append(tx.BodyBuf, chunk...)

	if !last {
		// Intermediate chunk — stay in BDAT mode.
		*inBdat = true
		resumeCh <- struct{}{}
		return &Response{250, "2.0.0 OK"}
	}

	// LAST chunk — message complete.  RFC 3030 Section 3: CHUNKING implies
	// BINARYMIME, so no 8BITMIME check needed (unlike DATA).
	*inBdat = false
	resumeCh <- struct{}{}

	s.logInfo("bdat_received",
		slog.Int("bytes", len(tx.BodyBuf)),
		slog.String("mail_from", tx.MailFrom),
		slog.Int("recipients", len(tx.Accepted)),
	)

	resp := s.Handler.Data(context.Background(), tx, tx.BodyBuf)

	if resp != nil && resp.Code == 250 && s.PostcatDir != "" {
		if path, err := postcat.Write(s.PostcatDir, tx.MailFrom, tx.Accepted, tx.BodyBuf); err != nil {
			s.logError("postcat_write_error",
				slog.String("error", err.Error()),
				slog.String("path", path),
			)
		}
	}

	tx.BodyBuf = nil
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
	if !strings.HasPrefix(strings.ToUpper(rest), "FROM:") {
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
	if !strings.HasPrefix(strings.ToUpper(rest), "TO:") {
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
	mu      sync.Mutex // guards writes
}

func (c *connState) SetReadDeadline(t time.Time) {
	_ = c.netConn.SetReadDeadline(t)
}

func (c *connState) write(s string, timeout time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if timeout > 0 {
		_ = c.netConn.SetWriteDeadline(time.Now().Add(timeout))
	}
	_, _ = c.w.WriteString(s)
	_ = c.w.Flush()
	if timeout > 0 {
		_ = c.netConn.SetWriteDeadline(time.Time{})
	}
}

func (c *connState) Close() error {
	return c.netConn.Close()
}

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
	line = strings.TrimRight(line, "\r\n")
	return line, nil
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
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

// newTx creates a fresh transaction state for a new connection.
func (s *Server) newTx(conn net.Conn) *Tx {
	return &Tx{
		RemoteAddr: conn.RemoteAddr(),
		Hostname:   s.Hostname,
		Params:     map[string]string{},
	}
}
