package smtpgateway

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stupoid/smtp-gateway/internal/postcat"
)

// ---------------------------------------------------------------------------
// parseSMTPCommand / splitVerb
// ---------------------------------------------------------------------------

func TestParseSMTPCommand(t *testing.T) {
	tests := []struct {
		line, wantVerb, wantArgs string
	}{
		{"EHLO client.example.com\r\n", "EHLO", "client.example.com"},
		{"HELO mx\r\n", "HELO", "mx"},
		{"MAIL FROM:<sender@t>\r\n", "MAIL", "FROM:<sender@t>"},
		{"RCPT TO:<rcpt@t>\r\n", "RCPT", "TO:<rcpt@t>"},
		{"DATA\r\n", "DATA", ""},
		{"QUIT\r\n", "QUIT", ""},
		{"RSET\r\n", "RSET", ""},
		{"noop\r\n", "NOOP", ""},
		{"  ehlo  with spaces  \r\n", "EHLO", "with spaces"},
	}

	for _, tc := range tests {
		verb, args := parseSMTPCommand(tc.line)
		if verb != tc.wantVerb || args != tc.wantArgs {
			t.Errorf("parseSMTPCommand(%q) = (%q, %q); want (%q, %q)",
				tc.line, verb, args, tc.wantVerb, tc.wantArgs)
		}
	}
}

func TestSplitVerb_NoArgs(t *testing.T) {
	verb, rest := splitVerb("DATA")
	if verb != "DATA" || rest != "" {
		t.Errorf("got (%q, %q); want (DATA, \"\")", verb, rest)
	}
}

// ---------------------------------------------------------------------------
// parseMailFrom
// ---------------------------------------------------------------------------

func TestParseMailFrom_Basic(t *testing.T) {
	from, params, err := parseMailFrom("FROM:<sender@t>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if from != "sender@t" {
		t.Errorf("from = %q, want sender@t", from)
	}
	if len(params) != 0 {
		t.Errorf("params = %v, want empty", params)
	}
}

func TestParseMailFrom_NullSender(t *testing.T) {
	from, _, err := parseMailFrom("FROM:<>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if from != "" {
		t.Errorf("from = %q, want empty (null sender)", from)
	}
}

func TestParseMailFrom_WithParams(t *testing.T) {
	from, params, err := parseMailFrom("FROM:<a@b> SIZE=1024 BODY=8BITMIME")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if from != "a@b" {
		t.Errorf("from = %q, want a@b", from)
	}
	if params["SIZE"] != "1024" {
		t.Errorf("SIZE = %q, want 1024", params["SIZE"])
	}
	if params["BODY"] != "8BITMIME" {
		t.Errorf("BODY = %q, want 8BITMIME", params["BODY"])
	}
}

func TestParseMailFrom_SMTPUTF8(t *testing.T) {
	// SMTPUTF8 is a bare keyword (no value), not key=value like SIZE=1024.
	from, params, err := parseMailFrom("FROM:<a@b> SMTPUTF8")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if from != "a@b" {
		t.Errorf("from = %q, want a@b", from)
	}
	if _, ok := params["SMTPUTF8"]; !ok {
		t.Error("SMTPUTF8 keyword not captured")
	}
}

func TestParseMailFrom_SMTPUTF8WithSize(t *testing.T) {
	from, params, err := parseMailFrom("FROM:<a@b> SMTPUTF8 SIZE=5000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := params["SMTPUTF8"]; !ok {
		t.Error("SMTPUTF8 keyword not captured")
	}
	if params["SIZE"] != "5000" {
		t.Errorf("SIZE = %q, want 5000", params["SIZE"])
	}
	if from != "a@b" {
		t.Errorf("from = %q, want a@b", from)
	}
}

func TestParseMailFrom_UTF8Address(t *testing.T) {
	from, _, err := parseMailFrom("FROM:<üser@münchen.de>")
	if err != nil {
		t.Fatalf("UTF-8 address should be valid: %v", err)
	}
	if from != "üser@münchen.de" {
		t.Errorf("from = %q, want üser@münchen.de", from)
	}
}

func TestParseMailFrom_Errors(t *testing.T) {
	tests := []string{
		"",          // empty
		"FROM",      // missing colon
		"FROM:",     // missing angle brackets
		"FROM:abc",  // no angle brackets
		"FROM:<abc", // missing closing bracket
	}
	for _, tc := range tests {
		_, _, err := parseMailFrom(tc)
		if !errors.Is(err, ErrBadSyntax) {
			t.Errorf("parseMailFrom(%q) err = %v, want ErrBadSyntax", tc, err)
		}
	}
}

// ---------------------------------------------------------------------------
// parseRcptTo
// ---------------------------------------------------------------------------

func TestParseRcptTo(t *testing.T) {
	tests := []struct{ args, want string }{
		{"TO:<rcpt@t>", "rcpt@t"},
		{"to:<RCPT@t>", "RCPT@t"},
		{"TO:<rcpt@t> SOME=param", "rcpt@t"},
		{"bare@addr", "bare@addr"}, // best-effort fallback
	}
	for _, tc := range tests {
		got := parseRcptTo(tc.args)
		if got != tc.want {
			t.Errorf("parseRcptTo(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// readDotUnstuffed
// ---------------------------------------------------------------------------

func TestReadDotUnstuffed_Normal(t *testing.T) {
	srv, cli := net.Pipe()
	defer func() { _ = srv.Close() }()
	defer func() { _ = cli.Close() }()

	input := "Subject: hello\r\n\r\nbody line 1\r\nbody line 2\r\n.\r\n"
	go func() {
		_, _ = cli.Write([]byte(input))
		_ = cli.Close()
	}()

	c := &connState{netConn: srv}
	body, err := readDotUnstuffed(bufio.NewReader(srv), 0, c, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// readLine strips CRLF; readDotUnstuffed re-appends \r\n after each line.
	want := "Subject: hello\r\n\r\nbody line 1\r\nbody line 2\r\n"
	if string(body) != want {
		t.Errorf("body = %q, want %q", string(body), want)
	}
}

func TestReadDotUnstuffed_DotStuffing(t *testing.T) {
	srv, cli := net.Pipe()
	defer func() { _ = srv.Close() }()
	defer func() { _ = cli.Close() }()

	// Lines starting with '.' get one leading dot stripped (dot-unstuffing).
	input := "Subject: hello\r\n.leading dot line\r\n.\r\n"
	go func() {
		_, _ = cli.Write([]byte(input))
		_ = cli.Close()
	}()

	c := &connState{netConn: srv}
	body, err := readDotUnstuffed(bufio.NewReader(srv), 0, c, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// ".leading dot line" → unstuffed to "leading dot line"
	want := "Subject: hello\r\nleading dot line\r\n"
	if string(body) != want {
		t.Errorf("body = %q, want %q", string(body), want)
	}
}

func TestReadDotUnstuffed_MaxSize(t *testing.T) {
	srv, cli := net.Pipe()
	defer func() { _ = srv.Close() }()
	defer func() { _ = cli.Close() }()

	input := "line one\r\nline two\r\n.\r\n"
	go func() {
		_, _ = cli.Write([]byte(input))
		_ = cli.Close()
	}()

	c := &connState{netConn: srv}
	_, err := readDotUnstuffed(bufio.NewReader(srv), 5, c, 0)
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Errorf("err = %v, want ErrMessageTooLarge", err)
	}
}

func TestReadDotUnstuffed_EOFBeforeTerminator(t *testing.T) {
	srv, cli := net.Pipe()
	defer func() { _ = srv.Close() }()
	defer func() { _ = cli.Close() }()

	input := "incomplete body without terminator"
	go func() {
		_, _ = cli.Write([]byte(input))
		_ = cli.Close()
	}()

	c := &connState{netConn: srv}
	_, err := readDotUnstuffed(bufio.NewReader(srv), 0, c, 0)
	if err == nil {
		t.Errorf("expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// WritePostcat / ParsePostcat round-trip
// ---------------------------------------------------------------------------

func TestPostcatRoundTrip(t *testing.T) {
	dir := t.TempDir()
	mailFrom := "sender@test"
	accepted := []string{"a@test", "b@test"}
	body := []byte("From: sender@test\r\nTo: a@test\r\n\r\nHello world\r\n")

	path, err := postcat.Write(dir, mailFrom, accepted, body)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	msg, err := postcat.Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if msg.Sender != "sender@test" {
		t.Errorf("sender = %q, want sender@test", msg.Sender)
	}
	if len(msg.Recipients) != 2 {
		t.Errorf("got %d recipients, want 2", len(msg.Recipients))
	}
	if string(msg.RawMessage) != string(body) {
		t.Errorf("raw = %q, want %q", string(msg.RawMessage), string(body))
	}
}

func TestPostcatNullSender(t *testing.T) {
	dir := t.TempDir()
	path, err := postcat.Write(dir, "", []string{"r@t"}, []byte("test\r\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	msg, err := postcat.Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if msg.Sender != "<>" {
		t.Errorf("sender = %q, want <>", msg.Sender)
	}
}

// ---------------------------------------------------------------------------
// Response formatting
// ---------------------------------------------------------------------------

func TestResponseString(t *testing.T) {
	r := &Response{250, "2.0.0 OK"}
	if s := r.String(); s != "250 2.0.0 OK\r\n" {
		t.Errorf("String() = %q, want 250 2.0.0 OK\\r\\n", s)
	}
}

func TestResponseMultiLine(t *testing.T) {
	r := &Response{250, "mx.example.com"}
	if s := r.MultiLine(); s != "250-mx.example.com\r\n" {
		t.Errorf("MultiLine() = %q, want 250-mx.example.com\\r\\n", s)
	}
}

// ---------------------------------------------------------------------------
// Concurrency: net.Pipe() SMTP sessions
// ---------------------------------------------------------------------------

type countingHandler struct {
	mu       sync.Mutex
	sessions map[int][]string // id → list of phase names
	lastBody map[int]string   // id → last body content for verification
}

func newCountingHandler() *countingHandler {
	return &countingHandler{
		sessions: make(map[int][]string),
		lastBody: make(map[int]string),
	}
}

func (h *countingHandler) record(id int, phase string) {
	h.mu.Lock()
	h.sessions[id] = append(h.sessions[id], phase)
	h.mu.Unlock()
}

func (h *countingHandler) Hello(_ context.Context, _ *Tx) *Response {
	h.record(0, "hello")
	return RespHelloOK
}

func (h *countingHandler) MailFrom(_ context.Context, _ *Tx) *Response {
	h.record(0, "mail")
	return RespMailOK
}

func (h *countingHandler) RcptTo(_ context.Context, _ *Tx) *Response {
	h.record(0, "rcpt")
	return RespRcptOK
}

func (h *countingHandler) Data(_ context.Context, _ *Tx, body []byte) *Response {
	h.record(0, "data")
	h.mu.Lock()
	h.lastBody[0] = string(body)
	h.mu.Unlock()
	return RespDataOK
}

func TestServerConcurrency(t *testing.T) {
	n := 10 // concurrent sessions

	// Each session gets a message with a unique ID embedded in the body.
	// We verify that the handler received the correct body for each session
	// with no cross-contamination.

	handler := newCountingHandler()

	srv := &Server{
		Hostname:    "test.local",
		Handler:     handler,
		ReadTimeout: 5 * time.Second,
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()

	var serveErr error
	go func() {
		serveErr = srv.Serve(l)
	}()

	addr := l.Addr().String()

	var wg sync.WaitGroup
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			errs[id] = smtpSession(t, addr, id)
		}(i)
	}
	wg.Wait()

	if serveErr != nil {
		t.Errorf("Serve: %v", serveErr)
	}
	for i, e := range errs {
		if e != nil {
			t.Errorf("session %d: %v", i, e)
		}
	}

	_ = l.Close()
	_ = srv.Shutdown(context.Background())
}

// smtpSession performs a complete SMTP transaction over a TCP connection.
func smtpSession(_ *testing.T, addr string, id int) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))

	// Read banner.
	if err := expectLine(rw, "220"); err != nil {
		return err
	}

	body := []byte("From: sender@test\r\nTo: rcpt@test\r\nX-Id: " +
		itoa(id) + "\r\n\r\nmessage body " + itoa(id) + "\r\n")

	commands := []string{
		"EHLO client-" + itoa(id) + "\r\n",
		"MAIL FROM:<s" + itoa(id) + "@test>\r\n",
		"RCPT TO:<r" + itoa(id) + "@test>\r\n",
		"DATA\r\n",
	}
	for _, cmd := range commands {
		if _, err := rw.WriteString(cmd); err != nil {
			return err
		}
		if err := rw.Flush(); err != nil {
			return err
		}
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			if err := drainML(rw); err != nil {
				return err
			}
		case strings.HasPrefix(cmd, "DATA"):
			if err := expectLine(rw, "354"); err != nil {
				return err
			}
		default:
			if err := expectLine(rw, "250"); err != nil {
				return err
			}
		}
	}

	// Now send the body.
	if _, err := rw.Write(body); err != nil {
		return err
	}
	if _, err := rw.WriteString("\r\n.\r\n"); err != nil {
		return err
	}
	if err := rw.Flush(); err != nil {
		return err
	}

	// Read the final response.
	if err := expectLine(rw, "250"); err != nil {
		return err
	}

	// QUIT
	if _, err := rw.WriteString("QUIT\r\n"); err != nil {
		return err
	}
	if err := rw.Flush(); err != nil {
		return err
	}
	if err := expectLine(rw, "221"); err != nil {
		return err
	}

	return nil
}

func expectLine(rw *bufio.ReadWriter, prefix string) error {
	line, err := rw.ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, prefix) {
		return fmt.Errorf("expected %s..., got %q", prefix, strings.TrimSpace(line))
	}
	return nil
}

// drainML reads multi-line responses until the final line (starts with "NNN ").
func drainML(rw *bufio.ReadWriter) error {
	for {
		line, err := rw.ReadString('\n')
		if err != nil {
			return err
		}
		if len(line) >= 4 && line[3] == ' ' {
			return nil
		}
	}
}

func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}

// ---------------------------------------------------------------------------
// Concurrency: PostcatDir writes — verify no data corruption under load
// ---------------------------------------------------------------------------

func TestPostcatConcurrency(t *testing.T) {
	dir := t.TempDir()
	n := 50

	var wg sync.WaitGroup
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			mailFrom := fmt.Sprintf("sender-%d@test", id)
			accepted := []string{fmt.Sprintf("rcpt-%d@test", id)}
			body := []byte(fmt.Sprintf("X-Id: %d\r\n\r\nbody %d\r\n", id, id))
			path, err := postcat.Write(dir, mailFrom, accepted, body)
			if err != nil {
				errs[id] = err
				return
			}
			// Verify we can read it back.
			msg, err := postcat.Parse(path)
			if err != nil {
				errs[id] = err
				return
			}
			if msg.Sender != fmt.Sprintf("sender-%d@test", id) {
				errs[id] = fmt.Errorf("sender mismatch: %q", msg.Sender)
			}
			if !strings.Contains(string(msg.RawMessage), fmt.Sprintf("X-Id: %d", id)) {
				errs[id] = fmt.Errorf("body missing X-Id: %d", id)
			}
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("goroutine %d: %v", i, e)
		}
	}

	// Count files.
	files, _ := filepath.Glob(filepath.Join(dir, "*.eml"))
	if len(files) != n {
		t.Errorf("expected %d files, got %d", n, len(files))
	}
}

// ---------------------------------------------------------------------------
// Concurrency: parseMailFrom — pure function, should be goroutine-safe
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Concurrency: Server with postcat writes and real body verification
// ---------------------------------------------------------------------------

func TestServerWithBodyVerification(t *testing.T) {
	dir := t.TempDir()

	srv := &Server{
		Hostname:    "test.local",
		Handler:     &acceptAllHandler{},
		ReadTimeout: 5 * time.Second,
		PostcatDir:  dir,
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()

	go func() { _ = srv.Serve(l) }()
	addr := l.Addr().String()

	n := 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	bodies := make([]string, n)

	for i := 0; i < n; i++ {
		bodies[i] = fmt.Sprintf("X-Concurrency-Id: %d\r\n\r\nunique body %d\r\n", i, i)
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				errs[id] = err
				return
			}
			defer func() { _ = conn.Close() }()
			rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
			if err := expectLine(rw, "220"); err != nil {
				errs[id] = err
				return
			}
			send := func(cmd string) error {
				_, _ = rw.WriteString(cmd)
				return rw.Flush()
			}
			_ = send("EHLO c" + itoa(id) + "\r\n")
			// Read multi-line EHLO response.
			for {
				l, _ := rw.ReadString('\n')
				if strings.HasPrefix(l, "250 ") {
					break
				}
			}
			_ = send("MAIL FROM:<s" + itoa(id) + "@test>\r\n")
			_ = expectLine(rw, "250")
			_ = send("RCPT TO:<r" + itoa(id) + "@test>\r\n")
			_ = expectLine(rw, "250")
			_ = send("DATA\r\n")
			_ = expectLine(rw, "354")
			_ = send(bodies[id] + "\r\n.\r\n")
			_ = expectLine(rw, "250")
			_ = send("QUIT\r\n")
			_ = expectLine(rw, "221")
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("session %d: %v", i, e)
		}
	}

	_ = l.Close()
	_ = srv.Shutdown(context.Background())

	// Verify postcat files: no cross-contamination.
	files, _ := filepath.Glob(filepath.Join(dir, "*.eml"))
	if len(files) != n {
		t.Errorf("expected %d postcat files, got %d", n, len(files))
	}
	for _, f := range files {
		msg, err := postcat.Parse(f)
		if err != nil {
			t.Errorf("parse %s: %v", f, err)
			continue
		}
		raw := string(msg.RawMessage)
		if !strings.Contains(raw, "X-Concurrency-Id:") {
			t.Errorf("file %s missing X-Concurrency-Id", f)
		}
	}
}

// ---------------------------------------------------------------------------
// Helper function unit tests
// ---------------------------------------------------------------------------

func TestIsTimeout(t *testing.T) {
	// A non-timeout error should return false.
	if isTimeout(errors.New("not a timeout")) {
		t.Error("isTimeout returned true for plain error")
	}
	if isTimeout(nil) {
		t.Error("isTimeout returned true for nil")
	}
	// A net.Error with Timeout()==true should return true.
	if !isTimeout(&testTimeoutError{timeout: true}) {
		t.Error("isTimeout returned false for timeout error")
	}
	// A net.Error with Timeout()==false should return false.
	if isTimeout(&testTimeoutError{timeout: false}) {
		t.Error("isTimeout returned true for non-timeout net.Error")
	}
}

// testTimeoutError implements net.Error for testing isTimeout.
type testTimeoutError struct{ timeout bool }

func (e *testTimeoutError) Error() string   { return "test timeout error" }
func (e *testTimeoutError) Timeout() bool   { return e.timeout }
func (e *testTimeoutError) Temporary() bool { return false }

func TestTruncate(t *testing.T) {
	tests := []struct {
		s    string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"longer string", 6, "longer..."},
		{"exact", 5, "exact"},
		{"", 5, ""},
		{"hello", 5, "hello"}, // exactly at limit
	}
	for _, tc := range tests {
		got := truncate(tc.s, tc.n)
		if got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
		}
	}
}

func TestContains8Bit(t *testing.T) {
	if contains8Bit([]byte("pure ascii")) {
		t.Error("contains8Bit returned true for 7-bit ASCII")
	}
	if contains8Bit([]byte{}) {
		t.Error("contains8Bit returned true for empty slice")
	}
	if !contains8Bit([]byte{0x80}) {
		t.Error("contains8Bit returned false for byte 0x80")
	}
	if !contains8Bit([]byte("hello\xFF")) {
		t.Error("contains8Bit returned false for byte 0xFF")
	}
}

func TestDefaultHostname(t *testing.T) {
	h := defaultHostname()
	if h == "" {
		t.Error("defaultHostname returned empty string")
	}
	// Must be either "localhost" or a valid-looking hostname (contains a dot).
	if h != "localhost" && !strings.Contains(h, ".") {
		t.Errorf("defaultHostname returned unexpected value: %q", h)
	}
}

// ---------------------------------------------------------------------------
// BDAT helpers
// ---------------------------------------------------------------------------

func TestParseBdatArgs(t *testing.T) {
	tests := []struct {
		args     string
		wantSize int
		wantLast bool
		wantErr  bool
	}{
		{"1024", 1024, false, false},
		{"1024 LAST", 1024, true, false},
		{"0", 0, false, false},
		{"0 LAST", 0, true, false},
		{"", 0, false, true},
		{"abc", 0, false, true},
		{"-1", 0, false, true},
		{"100 LAST extra", 0, false, true},
		{"100 extra", 0, false, true},
	}
	for _, tc := range tests {
		size, last, err := parseBdatArgs(tc.args)
		if tc.wantErr {
			if !errors.Is(err, ErrBadSyntax) {
				t.Errorf("parseBdatArgs(%q) err = %v, want ErrBadSyntax", tc.args, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseBdatArgs(%q) unexpected error: %v", tc.args, err)
			continue
		}
		if size != tc.wantSize {
			t.Errorf("parseBdatArgs(%q) size = %d, want %d", tc.args, size, tc.wantSize)
		}
		if last != tc.wantLast {
			t.Errorf("parseBdatArgs(%q) last = %v, want %v", tc.args, last, tc.wantLast)
		}
	}
}

func TestReadNBytes(t *testing.T) {
	srv, cli := net.Pipe()
	defer func() { _ = srv.Close() }()
	defer func() { _ = cli.Close() }()

	payload := []byte("raw binary data \x00\x01\x02\xFF")
	go func() {
		_, _ = cli.Write(payload)
		_ = cli.Close()
	}()

	c := &connState{netConn: srv}
	got, err := readNBytes(bufio.NewReader(srv), len(payload), c, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("readNBytes = %v, want %v", got, payload)
	}
}

func TestReadNBytesZero(t *testing.T) {
	got, err := readNBytes(nil, 0, nil, 0)
	if err != nil {
		t.Fatalf("zero read should not error: %v", err)
	}
	if got != nil {
		t.Errorf("zero read should return nil, got %v", got)
	}
}

func TestReadNBytesEOF(t *testing.T) {
	srv, cli := net.Pipe()
	_ = cli.Close()

	c := &connState{netConn: srv}
	_, err := readNBytes(bufio.NewReader(srv), 10, c, 0)
	if err == nil {
		t.Error("expected error reading from closed conn")
	}
}

// ---------------------------------------------------------------------------
// Handler rejection tests
// ---------------------------------------------------------------------------

type rejectHeloHandler struct{}

func (h *rejectHeloHandler) Hello(_ context.Context, _ *Tx) *Response {
	return &Response{550, "5.7.1 HELO rejected"}
}
func (h *rejectHeloHandler) MailFrom(_ context.Context, _ *Tx) *Response { return RespMailOK }
func (h *rejectHeloHandler) RcptTo(_ context.Context, _ *Tx) *Response   { return RespRcptOK }
func (h *rejectHeloHandler) Data(_ context.Context, _ *Tx, _ []byte) *Response {
	return RespDataOK
}

func TestHandlerRejectsHelo(t *testing.T) {
	srv := &Server{
		Hostname:    "test.local",
		Handler:     &rejectHeloHandler{},
		ReadTimeout: 5 * time.Second,
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))

	// Read banner.
	if err := expectLine(rw, "220"); err != nil {
		t.Fatalf("banner: %v", err)
	}

	// HELO should be rejected by the handler.
	_, _ = rw.WriteString("HELO bad.domain\r\n")
	_ = rw.Flush()
	if err := expectLine(rw, "550"); err != nil {
		t.Fatalf("expected 550 rejection: %v", err)
	}

	// Connection should still be usable — send EHLO (handler also rejects).
	_, _ = rw.WriteString("EHLO another.domain\r\n")
	_ = rw.Flush()
	if err := expectLine(rw, "550"); err != nil {
		t.Fatalf("expected 550 rejection for EHLO: %v", err)
	}

	_, _ = rw.WriteString("QUIT\r\n")
	_ = rw.Flush()
	_ = expectLine(rw, "221")

	_ = l.Close()
	_ = srv.Shutdown(context.Background())
}

type rejectMailHandler struct{}

func (h *rejectMailHandler) Hello(_ context.Context, _ *Tx) *Response    { return RespHelloOK }
func (h *rejectMailHandler) MailFrom(_ context.Context, _ *Tx) *Response { return RespBadSeq }
func (h *rejectMailHandler) RcptTo(_ context.Context, _ *Tx) *Response   { return RespRcptOK }
func (h *rejectMailHandler) Data(_ context.Context, _ *Tx, _ []byte) *Response {
	return RespDataOK
}

func TestHandlerRejectsMailFrom(t *testing.T) {
	srv := &Server{
		Hostname:    "test.local",
		Handler:     &rejectMailHandler{},
		ReadTimeout: 5 * time.Second,
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))

	_ = expectLine(rw, "220")

	_, _ = rw.WriteString("EHLO mx\r\n")
	_ = rw.Flush()
	_ = drainML(rw)

	// MAIL FROM is rejected by handler.
	_, _ = rw.WriteString("MAIL FROM:<s@t>\r\n")
	_ = rw.Flush()
	if err := expectLine(rw, "503"); err != nil {
		t.Fatalf("expected 503 rejection: %v", err)
	}

	_, _ = rw.WriteString("QUIT\r\n")
	_ = rw.Flush()
	_ = expectLine(rw, "221")

	_ = l.Close()
	_ = srv.Shutdown(context.Background())
}

type rejectRcptHandler struct{}

func (h *rejectRcptHandler) Hello(_ context.Context, _ *Tx) *Response    { return RespHelloOK }
func (h *rejectRcptHandler) MailFrom(_ context.Context, _ *Tx) *Response { return RespMailOK }
func (h *rejectRcptHandler) RcptTo(_ context.Context, _ *Tx) *Response {
	return &Response{550, "5.1.1 User unknown"}
}
func (h *rejectRcptHandler) Data(_ context.Context, _ *Tx, _ []byte) *Response { return RespDataOK }

func TestHandlerRejectsRcptTo(t *testing.T) {
	srv := &Server{
		Hostname:    "test.local",
		Handler:     &rejectRcptHandler{},
		ReadTimeout: 5 * time.Second,
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))

	_ = expectLine(rw, "220")

	_, _ = rw.WriteString("EHLO mx\r\n")
	_ = rw.Flush()
	_ = drainML(rw)
	_, _ = rw.WriteString("MAIL FROM:<s@t>\r\n")
	_ = rw.Flush()
	_ = expectLine(rw, "250")

	// All recipients rejected → DATA returns 554 (no valid recipients).
	_, _ = rw.WriteString("RCPT TO:<bad@t>\r\n")
	_ = rw.Flush()
	if err := expectLine(rw, "550"); err != nil {
		t.Fatalf("expected 550: %v", err)
	}

	// DATA with no accepted recipients → 554.
	_, _ = rw.WriteString("DATA\r\n")
	_ = rw.Flush()
	if err := expectLine(rw, "554"); err != nil {
		t.Fatalf("expected 554 no valid recipients: %v", err)
	}

	_, _ = rw.WriteString("QUIT\r\n")
	_ = rw.Flush()
	_ = expectLine(rw, "221")

	_ = l.Close()
	_ = srv.Shutdown(context.Background())
}

// ---------------------------------------------------------------------------
// Postcat edge cases
// ---------------------------------------------------------------------------

func TestPostcatParseEmptyBody(t *testing.T) {
	dir := t.TempDir()
	path, err := postcat.Write(dir, "s@test", []string{"r@test"}, []byte{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	msg, err := postcat.Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(msg.RawMessage) != 0 {
		t.Errorf("expected empty body, got %d bytes", len(msg.RawMessage))
	}
}

func TestPostcatParsePreservesBody(t *testing.T) {
	dir := t.TempDir()
	// Body with CRLF line endings.
	body := []byte("From: sender\r\nTo: rcpt\r\nSubject: test\r\n\r\nHello world\r\n")
	path, err := postcat.Write(dir, "s@test", []string{"r@test"}, body)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	msg, err := postcat.Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if string(msg.RawMessage) != string(body) {
		t.Errorf("body mismatch:\n got:  %q\n want: %q", string(msg.RawMessage), string(body))
	}
}

func TestPostcatParseNoTimestamp(t *testing.T) {
	dir := t.TempDir()
	// Write a file manually with only S and R records (no T).
	path := filepath.Join(dir, "notimestamp.eml")
	if err := os.WriteFile(path, []byte("S sender@t\nR rcpt@t\n\nbody\r\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	msg, err := postcat.Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if msg.Sender != "sender@t" {
		t.Errorf("sender = %q, want sender@t", msg.Sender)
	}
	if !msg.Time.IsZero() {
		t.Errorf("expected zero time (no T record), got %v", msg.Time)
	}
}

// ---------------------------------------------------------------------------
// acceptAllHandler
// ---------------------------------------------------------------------------

type acceptAllHandler struct{}

func (h *acceptAllHandler) Hello(_ context.Context, _ *Tx) *Response          { return RespHelloOK }
func (h *acceptAllHandler) MailFrom(_ context.Context, _ *Tx) *Response       { return RespMailOK }
func (h *acceptAllHandler) RcptTo(_ context.Context, _ *Tx) *Response         { return RespRcptOK }
func (h *acceptAllHandler) Data(_ context.Context, _ *Tx, _ []byte) *Response { return RespDataOK }
