// Package smtpgateway provides an SMTP server library with per-phase hooks.
// Users implement the Handler interface to accept/reject at each SMTP
// transaction phase and route message data to arbitrary sinks.
package smtpgateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"
)

// Response is an SMTP response code and message.
type Response struct {
	Code    int
	Message string
}

// String formats the response as an SMTP status line (ends in \r\n).
func (r *Response) String() string {
	return fmt.Sprintf("%d %s\r\n", r.Code, r.Message)
}

// MultiLine formats the response for use in a multi-line reply.
// Lines after the first use the code followed by a dash.
func (r *Response) MultiLine() string {
	// Formats a single continuation line of a multi-line SMTP reply.
	// Each continuation uses code-dash; the final line must use
	// String() (code-space) instead.  See handleEhlo for the pattern.
	return fmt.Sprintf("%d-%s\r\n", r.Code, r.Message)
}

// Copy returns a newly allocated copy of the Response.
// Use Resp*.Copy() when you need to mutate a pre-defined response
// (e.g. RespMailOK.Copy()).  The Resp* variables are shared
// pointers and must not be modified directly.
func (r *Response) Copy() *Response {
	return &Response{Code: r.Code, Message: r.Message}
}

// Pre-defined SMTP responses. These are shared pointers -- do not modify
// them. Use Resp*.Copy() if you need a mutable version.
var (
	RespHelloOK      = &Response{250, "OK"}
	RespMailOK       = &Response{250, "2.1.0 OK"}
	RespRcptOK       = &Response{250, "2.1.5 OK"}
	RespDataOK       = &Response{250, "2.0.0 OK"}
	RespGoodbye      = &Response{221, "2.0.0 Bye"}
	RespStartMail    = &Response{354, "Go ahead"}
	RespBadSeq       = &Response{503, "5.5.1 Bad sequence of commands"}
	RespSysError     = &Response{451, "4.3.0 System error"}
	RespVrfyDisabled = &Response{252, "2.0.0 VRFY not supported"}
	RespMessageSize  = &Response{552, "5.3.4 Message size exceeds fixed limit"}
	RespEightBit     = &Response{550, "5.6.3 Message contains 8-bit data but BODY=8BITMIME not specified"}
)

// Default values applied by NewServer and Serve() for zero-value fields.
const (
	DefaultMaxMessageSize = 25 << 20 // 25 MiB
	DefaultReadTimeout    = 5 * time.Minute
	DefaultIdleTimeout    = 5 * time.Minute
)

// Rejection records a recipient that was rejected at the RCPT TO phase.
type Rejection struct {
	Recipient string
	Response  *Response
}

// Tx accumulates state across the phases of a single SMTP transaction.
// It is passed to each callback on the Handler and is reset on RSET.
type Tx struct {
	RemoteAddr net.Addr
	TLS        *tls.ConnectionState
	Hostname   string // server's advertised hostname (from Server.Hostname, set before EHLO)
	Helo       string // client's HELO/EHLO domain
	MailFrom   string // envelope sender (empty string for null sender <>)
	Params     map[string]string
	Rcpts      []string
	Accepted   []string
	Rejected   []Rejection
}

// Handler receives callbacks at each phase of an SMTP transaction.
// Implementations must be safe for concurrent calls on separate
// connections (one Handler is shared across all connections).
// Callbacks for a single connection are always serial.
type Handler interface {
	// Hello is called after the client sends HELO or EHLO.
	// Return a non-250 response to reject the greeting.  The session
	// stays open, but it cannot start a transaction.
	Hello(ctx context.Context, tx *Tx) *Response

	// MailFrom is called after MAIL FROM.  The envelope sender is in
	// tx.MailFrom; any parameters (SIZE, BODY, etc.) are in tx.Params.
	MailFrom(ctx context.Context, tx *Tx) *Response

	// RcptTo is called for each RCPT TO.  The most recently presented
	// recipient is the last element of tx.Rcpts.  Return a non-250
	// response to reject this specific recipient; accepted ones remain
	// in tx.Accepted.
	RcptTo(ctx context.Context, tx *Tx) *Response

	// Data is called when the client sends the complete message body.
	// The body has been dot-unstuffed and is the raw RFC 5322 message
	// (headers + body).  This is called only if at least one recipient
	// was accepted.  Return a non-250 response to reject the message.
	Data(ctx context.Context, tx *Tx, body []byte) *Response
}

// Logger is the logging interface used by the server.
// A *slog.Logger satisfies this via the SlogLogger adapter below.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Error(msg string, args ...any)
}

// SlogLogger is a Logger backed by a *slog.Logger.
// Debug → slog.LevelDebug, Info → slog.LevelInfo, Error → slog.LevelError.
// A nil SlogLogger (or one with a nil Logger) is a no-op — all methods
// safely discard the call.
type SlogLogger struct {
	Logger *slog.Logger
}

// Debug logs at slog.LevelDebug.
func (a *SlogLogger) Debug(msg string, args ...any) {
	if a == nil || a.Logger == nil {
		return
	}
	a.Logger.Debug(msg, args...)
}

// Info logs at slog.LevelInfo.
func (a *SlogLogger) Info(msg string, args ...any) {
	if a == nil || a.Logger == nil {
		return
	}
	a.Logger.Info(msg, args...)
}

// Error logs at slog.LevelError.
func (a *SlogLogger) Error(msg string, args ...any) {
	if a == nil || a.Logger == nil {
		return
	}
	a.Logger.Error(msg, args...)
}

// Server is an SMTP gateway server.  It accepts connections from
// an existing net.Listener and dispatches each connection to the
// configured Handler.
//
// Zero-value fields get sensible defaults (see defaults set in Serve).
type Server struct {
	// Hostname used in the SMTP banner and EHLO response.
	// If empty, defaults to the system hostname.
	Hostname string

	// TLSConfig enables STARTTLS when non-nil.  The server
	// advertises STARTTLS in EHLO and upgrades the connection
	// when the client issues the command.
	//
	TLSConfig *tls.Config

	// RequireTLS rejects MAIL with 530 until the connection is protected
	// by TLS.  It requires TLSConfig.  The default is false: STARTTLS is
	// offered, but plaintext mail is accepted.
	RequireTLS bool

	// Handler receives callbacks for each SMTP transaction phase.
	// Required — Serve returns an error if nil.
	Handler Handler

	// MaxMessageSize is the maximum message size in bytes.
	// Defaults to 25 MiB (enforced in Serve).  The zero value is
	// overridden.  A negative value disables the limit and the SIZE
	// advertisement; do not use one.  SIZE is advertised in EHLO and
	// messages exceeding the limit are rejected during DATA and BDAT.
	MaxMessageSize int

	// MaxRecipients is the maximum number of RCPT TO commands
	// per transaction.  0 means unlimited.
	MaxRecipients int

	// MaxConnections limits concurrent connections.  0 means unlimited.
	//
	// Without a limit, an attacker can exhaust file descriptors and
	// goroutine memory by opening many idle connections.  Set a limit
	// appropriate for your deployment.
	MaxConnections int

	// ReadTimeout is the per-line read deadline.  Defaults to 5 minutes.
	// The timer is reset after each successful read.
	ReadTimeout time.Duration

	// WriteTimeout is the per-response write deadline.  0 means no deadline.
	WriteTimeout time.Duration

	// IdleTimeout is the intended inactivity limit.  Defaults to
	// 5 minutes.  The current code overwrites this deadline with
	// ReadTimeout before each read, so it has no effect when
	// ReadTimeout is non-zero.
	IdleTimeout time.Duration

	// Logger receives structured log events.  If nil, logging is
	// discarded.  Use SlogLogger to wrap a *slog.Logger, or use
	// NewSlogLogger() as a convenience constructor.
	Logger Logger

	// PostcatDir, when non-empty, causes the server to write each
	// accepted message as a Postfix-style file in this directory
	// (see internal/postcat).  The format is not the Postfix queue
	// file format.  The directory is created with mode 0750 if it
	// does not exist.
	//
	// Postcat writes are best-effort: the file is written after
	// Handler.Data returns 250.  If the write fails (disk full,
	// permission denied), the error is logged but the SMTP client
	// still receives 250 — the handler has already committed.
	// For guaranteed persistence, implement it in Handler.Data.
	PostcatDir string

	// --- internal ---
	ln           net.Listener
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	connSem      chan struct{}
	shutdownOnce sync.Once
}

// ServerOption configures a Server constructed via NewServer.
type ServerOption func(*Server)

// NewServer returns a fully initialised Server with handler set and all
// defaults applied.  Options override defaults; when the same option is
// passed multiple times, the last one wins.
//
// Hostname is resolved eagerly via os.Hostname() (falling back to
// "localhost").  WithHostname overrides it.  Struct-literal initialization
// (&Server{Handler: h}) defers hostname resolution to Serve() — reading
// s.Hostname before Serve() returns an empty string in that path.
//
// Struct-literal initialization remains supported — Serve() applies the
// same defaults for zero-value fields.
func NewServer(handler Handler, opts ...ServerOption) *Server {
	s := &Server{
		Handler:        handler,
		Hostname:       defaultHostname(),
		MaxMessageSize: DefaultMaxMessageSize,
		ReadTimeout:    DefaultReadTimeout,
		IdleTimeout:    DefaultIdleTimeout,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// WithHostname sets the server hostname used in the SMTP banner and
// EHLO response.
func WithHostname(v string) ServerOption { return func(s *Server) { s.Hostname = v } }

// WithTLSConfig sets the TLS configuration for STARTTLS support.
func WithTLSConfig(v *tls.Config) ServerOption { return func(s *Server) { s.TLSConfig = v } }

// WithRequireTLS requires TLS before MAIL.  Serve returns an error when
// RequireTLS is true and TLSConfig is nil.
func WithRequireTLS(v bool) ServerOption { return func(s *Server) { s.RequireTLS = v } }

// WithMaxMessageSize sets the maximum message size in bytes.
func WithMaxMessageSize(v int) ServerOption { return func(s *Server) { s.MaxMessageSize = v } }

// WithMaxRecipients sets the maximum number of RCPT TO commands per
// transaction.  0 means unlimited.
func WithMaxRecipients(v int) ServerOption { return func(s *Server) { s.MaxRecipients = v } }

// WithMaxConnections limits concurrent connections.  0 means unlimited.
func WithMaxConnections(v int) ServerOption { return func(s *Server) { s.MaxConnections = v } }

// WithReadTimeout sets the per-line read deadline.
func WithReadTimeout(v time.Duration) ServerOption { return func(s *Server) { s.ReadTimeout = v } }

// WithWriteTimeout sets the per-response write deadline.  0 means no deadline.
func WithWriteTimeout(v time.Duration) ServerOption { return func(s *Server) { s.WriteTimeout = v } }

// WithIdleTimeout sets the connection idle timeout.
func WithIdleTimeout(v time.Duration) ServerOption { return func(s *Server) { s.IdleTimeout = v } }

// WithLogger sets the server logger.  Pass nil to discard logs.
func WithLogger(v Logger) ServerOption { return func(s *Server) { s.Logger = v } }

// WithPostcatDir sets the postcat output directory.  When non-empty,
// accepted messages are written as Postfix-style files.
func WithPostcatDir(v string) ServerOption { return func(s *Server) { s.PostcatDir = v } }

// NewSlogLogger returns a Logger backed by l.  If l is nil, the returned
// Logger is a no-op — all methods safely discard the call.
func NewSlogLogger(l *slog.Logger) Logger {
	return &SlogLogger{Logger: l}
}

// Serve accepts connections on ln and handles them until the listener
// is closed or Shutdown is called.  Serve blocks until the listener
// returns an error or Shutdown drains existing connections.
//
// The caller owns the listener and is responsible for closing it to
// stop Serve.  For TLS-wrapped listeners (SMTPS), the caller creates
// a tls.Listener before passing it to Serve.
func (s *Server) Serve(ln net.Listener) error {
	if s.Handler == nil {
		return errors.New("smtpgateway: Handler is nil")
	}
	if s.RequireTLS && s.TLSConfig == nil {
		return errors.New("smtpgateway: RequireTLS needs TLSConfig")
	}
	if s.PostcatDir != "" {
		if err := os.MkdirAll(s.PostcatDir, 0750); err != nil {
			return fmt.Errorf("smtpgateway: PostcatDir: %w", err)
		}
	}
	s.ln = ln
	s.ctx, s.cancel = context.WithCancel(context.Background())
	if s.MaxConnections > 0 {
		s.connSem = make(chan struct{}, s.MaxConnections)
	}
	if s.Hostname == "" {
		s.Hostname = defaultHostname()
	}
	if s.MaxMessageSize == 0 {
		s.MaxMessageSize = DefaultMaxMessageSize
	}
	if s.ReadTimeout == 0 {
		s.ReadTimeout = DefaultReadTimeout
	}
	if s.IdleTimeout == 0 {
		s.IdleTimeout = DefaultIdleTimeout
	}
	if s.Logger != nil {
		s.logInfo("max_message_size", slog.Int("bytes", s.MaxMessageSize))
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.ctx.Done():
				s.wg.Wait()
				return nil
			default:
				return err
			}
		}
		if s.MaxConnections > 0 {
			select {
			case s.connSem <- struct{}{}:
			default:
				_ = conn.Close()
				s.logError("max_connections_reached")
				continue
			}
		}
		s.wg.Add(1)
		go func(conn net.Conn) {
			defer s.wg.Done()
			defer func() {
				if s.MaxConnections > 0 {
					<-s.connSem
				}
			}()
			s.handleConn(conn)
		}(conn)
	}
}

// Shutdown cancels the server context and waits for active connections
// to finish or for ctx to expire.  It does not close the listener, so
// Serve keeps blocking in Accept until the caller closes it.  Calling
// Shutdown more than once is safe.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) logDebug(msg string, args ...any) {
	if s.Logger != nil {
		s.Logger.Debug(msg, args...)
	}
}

func (s *Server) logInfo(msg string, args ...any) {
	if s.Logger != nil {
		s.Logger.Info(msg, args...)
	}
}

func (s *Server) logError(msg string, args ...any) {
	if s.Logger != nil {
		s.Logger.Error(msg, args...)
	}
}

func defaultHostname() string {
	h, err := os.Hostname()
	if err == nil && h != "" {
		return h
	}
	return "localhost"
}
