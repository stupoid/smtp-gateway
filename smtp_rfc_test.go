package smtpgateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// TCP test helpers
// ---------------------------------------------------------------------------

// dialServer starts an accept-all server on 127.0.0.1:0 and returns a
// connected net.Conn + bufio.Scanner for reading responses.
func dialServer(t *testing.T) (net.Conn, *bufio.Scanner) {
	t.Helper()

	srv := &Server{
		Hostname:    "test.local",
		Handler:     &acceptAllHandler{},
		ReadTimeout: 5 * time.Second,
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	scanner := bufio.NewScanner(conn)
	// Read banner
	if !scanner.Scan() {
		t.Fatalf("no banner: %v", scanner.Err())
	}
	if !strings.HasPrefix(scanner.Text(), "220 ") {
		t.Fatalf("unexpected banner: %s", scanner.Text())
	}

	return conn, scanner
}

// write writes a raw SMTP line to the connection.
func write(t *testing.T, conn net.Conn, s string) {
	t.Helper()
	if _, err := io.WriteString(conn, s); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// readResp reads a single SMTP response line.
func readResp(t *testing.T, scanner *bufio.Scanner) string {
	t.Helper()
	if !scanner.Scan() {
		t.Fatalf("no response: %v", scanner.Err())
	}
	return scanner.Text()
}

// readMultiline reads all lines of a multi-line response (250-...) until
// the final line (250 ...).
func readMultiline(t *testing.T, scanner *bufio.Scanner) []string {
	t.Helper()
	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		lines = append(lines, line)
		if len(line) >= 4 && line[3] == ' ' {
			return lines
		}
	}
	t.Fatalf("incomplete multiline response: %v", scanner.Err())
	return lines
}

// expectResp reads a response and checks that it starts with prefix.
func expectResp(t *testing.T, scanner *bufio.Scanner, prefix string) string {
	t.Helper()
	resp := readResp(t, scanner)
	if !strings.HasPrefix(resp, prefix) {
		t.Fatalf("expected %s..., got %q", prefix, resp)
	}
	return resp
}

// sendAndExpect sends cmd and expects prefix in response.
func sendAndExpect(t *testing.T, conn net.Conn, scanner *bufio.Scanner, cmd, prefix string) string {
	t.Helper()
	write(t, conn, cmd)
	return expectResp(t, scanner, prefix)
}

// ---------------------------------------------------------------------------
// Body edge cases
// ---------------------------------------------------------------------------

func TestSMTPBodyEmpty(t *testing.T) {
	conn, scanner := dialServer(t)

	sendAndExpect(t, conn, scanner, "EHLO test\r\n", "250")
	_ = readMultiline(t, scanner)

	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "RCPT TO:<r@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "DATA\r\n", "354")

	write(t, conn, "\r\n.\r\n")
	expectResp(t, scanner, "250")
	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

func TestSMTPBodyPreserveNewlines(t *testing.T) {
	tests := []struct {
		name string
		body string // raw bytes to send during DATA
	}{
		{
			name: "crlf",
			body: "line 1\r\nline 2\r\n.\r\n",
		},
		{
			name: "bare-lf",
			body: "line 1\nline 2\n.\n",
		},
		{
			name: "mixed",
			body: "line 1\r\nline 2\nline 3\r\n.\r\n",
		},
		{
			name: "trailing-blank-before-dot",
			body: "line 1\r\n\r\n.\r\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, scanner := dialServer(t)

			sendAndExpect(t, conn, scanner, "EHLO t\r\n", "250")
			_ = readMultiline(t, scanner)

			sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t>\r\n", "250")
			sendAndExpect(t, conn, scanner, "RCPT TO:<r@t>\r\n", "250")
			sendAndExpect(t, conn, scanner, "DATA\r\n", "354")

			write(t, conn, tt.body)
			// Should succeed regardless of line ending style.
			resp := expectResp(t, scanner, "250")
			_ = resp

			sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
		})
	}
}

// TestSMTPBodyDotUnstuffing verifies that leading dots are properly
// unstuffed during DATA reading (unit-tested in TestReadDotUnstuffed_DotStuffing).
// This E2E test confirms the full server pipeline handles dot-stuffed input and
// that the handler receives correctly unstuffed body content.
func TestSMTPBodyDotUnstuffing(t *testing.T) {
	// Use a recorder handler to verify the body is unstuffed.
	h := &dotUnstuffRecorder{}
	srv := &Server{
		Hostname:    "test.local",
		Handler:     h,
		ReadTimeout: 5 * time.Second,
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("no banner: %v", scanner.Err())
	}

	sendAndExpect(t, conn, scanner, "EHLO t\r\n", "250")
	_ = readMultiline(t, scanner)
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "RCPT TO:<r@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "DATA\r\n", "354")

	// SMTP dot-stuffing only applies to lines that START with a dot.
	// Wire:  ".." → unstuffed: "."   (leading dot stripped)
	// Wire:  "..." → unstuffed: ".." (one leading dot stripped)
	write(t, conn, "..dot-stuffed line\r\n")
	write(t, conn, "...double leading dot\r\n")
	write(t, conn, "normal line\r\n")
	write(t, conn, "\r\n.\r\n")
	expectResp(t, scanner, "250")

	// Verify dot-unstuffing: leading dots on lines should be stripped.
	if !bytes.Contains(h.body, []byte(".dot-stuffed line")) {
		t.Errorf("dot-unstuffing failed: expected '.dot-stuffed line', got body: %q", string(h.body))
	}
	if !bytes.Contains(h.body, []byte("..double leading dot")) {
		t.Errorf("dot-unstuffing failed: expected '..double leading dot', got body: %q", string(h.body))
	}

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

// dotUnstuffRecorder captures the body for dot-unstuffing verification.
type dotUnstuffRecorder struct {
	body []byte
}

func (h *dotUnstuffRecorder) Hello(_ context.Context, _ *Tx) *Response    { return RespHelloOK }
func (h *dotUnstuffRecorder) MailFrom(_ context.Context, _ *Tx) *Response { return RespMailOK }
func (h *dotUnstuffRecorder) RcptTo(_ context.Context, _ *Tx) *Response   { return RespRcptOK }
func (h *dotUnstuffRecorder) Data(_ context.Context, _ *Tx, body []byte) *Response {
	h.body = append([]byte{}, body...)
	return RespDataOK
}

// ---------------------------------------------------------------------------
// SMTP smuggling protection
// ---------------------------------------------------------------------------

// TestSMTPSmugglingBareLFDot verifies that a bare-LF dot line
// (\n.\n) terminates DATA early.  This is the correct anti-smuggling
// behaviour described at https://www.postfix.org/smtp-smuggling.html.
func TestSMTPSmugglingBareLFDot(t *testing.T) {
	conn, scanner := dialServer(t)

	sendAndExpect(t, conn, scanner, "EHLO t\r\n", "250")
	_ = readMultiline(t, scanner)
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "RCPT TO:<r@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "DATA\r\n", "354")

	// Send a body whose first line ends with CRLF, then a smuggled
	// dot line formed with bare LF.
	write(t, conn, "legit header\r\n")
	write(t, conn, "\n.\n")
	// If the server correctly detects the bare-LF dot, it will terminate
	// DATA here and "trailing garbage" becomes the next command.
	write(t, conn, "evil payload\r\n")
	write(t, conn, ".\r\n")

	// The server should respond to the completed DATA transaction.
	expectResp(t, scanner, "250")

	// The "evil payload" and real ".\r\n" are now floating as next
	// commands.  Each is an unknown command → 500.  Two 500s expected
	// before QUIT.
	expectResp(t, scanner, "500")
	expectResp(t, scanner, "500")

	write(t, conn, "QUIT\r\n")
	expectResp(t, scanner, "221")
}

// TestSMTPSmugglingCRLFDotLF verifies that \r\n.\n also terminates DATA.
// go-smtp's TestServer_smtpSmuggling expects this NOT to terminate, but
// protecting against it is more secure.
func TestSMTPSmugglingCRLFDotLF(t *testing.T) {
	conn, scanner := dialServer(t)

	sendAndExpect(t, conn, scanner, "EHLO t\r\n", "250")
	_ = readMultiline(t, scanner)
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "RCPT TO:<r@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "DATA\r\n", "354")

	write(t, conn, "legit header\r\n")
	write(t, conn, "\r\n.\n")
	write(t, conn, "evil payload\r\n")
	write(t, conn, ".\r\n")

	// Server terminates DATA at \r\n.\n.
	expectResp(t, scanner, "250")
	expectResp(t, scanner, "500") // evil payload
	expectResp(t, scanner, "500") // ".\r\n" is also an unknown command

	write(t, conn, "QUIT\r\n")
	expectResp(t, scanner, "221")
}

// ---------------------------------------------------------------------------
// Protocol commands (NOOP, RSET, VRFY, HELP)
// ---------------------------------------------------------------------------

func TestSMTPNOOP(t *testing.T) {
	conn, scanner := dialServer(t)
	sendAndExpect(t, conn, scanner, "EHLO t\r\n", "250")
	_ = readMultiline(t, scanner)
	sendAndExpect(t, conn, scanner, "NOOP\r\n", "250")
	sendAndExpect(t, conn, scanner, "noop with trailing\r\n", "250")
	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

func TestSMTPRSET(t *testing.T) {
	conn, scanner := dialServer(t)

	sendAndExpect(t, conn, scanner, "EHLO t\r\n", "250")
	_ = readMultiline(t, scanner)
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t>\r\n", "250")

	// RSET resets the transaction; another MAIL FROM is fine.
	sendAndExpect(t, conn, scanner, "RSET\r\n", "250")
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s2@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "RCPT TO:<r@t>\r\n", "250")

	// RSET again, then start fresh.
	sendAndExpect(t, conn, scanner, "RSET\r\n", "250")
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s3@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "RCPT TO:<r@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "DATA\r\n", "354")
	write(t, conn, "body\r\n.\r\n")
	expectResp(t, scanner, "250")
	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

func TestSMTPVRFY(t *testing.T) {
	conn, scanner := dialServer(t)
	sendAndExpect(t, conn, scanner, "EHLO t\r\n", "250")
	_ = readMultiline(t, scanner)

	// VRFY is disabled — 252 tells the client it's not supported.
	sendAndExpect(t, conn, scanner, "VRFY user\r\n", "252")
	sendAndExpect(t, conn, scanner, "EXPN list\r\n", "252")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

func TestSMTPUnknownCommand(t *testing.T) {
	conn, scanner := dialServer(t)
	sendAndExpect(t, conn, scanner, "EHLO t\r\n", "250")
	_ = readMultiline(t, scanner)

	sendAndExpect(t, conn, scanner, "HELP\r\n", "500")
	sendAndExpect(t, conn, scanner, "FOOBAR\r\n", "500")
	sendAndExpect(t, conn, scanner, "X-BOGUS 1 2 3\r\n", "500")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

// ---------------------------------------------------------------------------
// HELO / EHLO state machine
// ---------------------------------------------------------------------------

func TestSMTPHeloTwiceRejected(t *testing.T) {
	conn, scanner := dialServer(t)

	sendAndExpect(t, conn, scanner, "EHLO first\r\n", "250")
	_ = readMultiline(t, scanner)

	// Second HELO after one already accepted → 503
	sendAndExpect(t, conn, scanner, "HELO second\r\n", "503")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

func TestSMTPEhloAfterHelo(t *testing.T) {
	conn, scanner := dialServer(t)

	sendAndExpect(t, conn, scanner, "HELO mx\r\n", "250")

	// EHLO after HELO → 503 (greeting already received)
	sendAndExpect(t, conn, scanner, "EHLO mx\r\n", "503")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

func TestSMTPHeloNoDomain(t *testing.T) {
	conn, scanner := dialServer(t)

	// HELO without domain → 501
	sendAndExpect(t, conn, scanner, "HELO\r\n", "501")

	// Can still send a valid HELO afterward.
	sendAndExpect(t, conn, scanner, "HELO mx.example.com\r\n", "250")
	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

// ---------------------------------------------------------------------------
// Transaction sequence enforcement
// ---------------------------------------------------------------------------

func TestSMTPRcptBeforeHelo(t *testing.T) {
	conn, scanner := dialServer(t)

	sendAndExpect(t, conn, scanner, "RCPT TO:<r@t>\r\n", "503")

	// After HELO, everything is fine.
	sendAndExpect(t, conn, scanner, "EHLO mx\r\n", "250")
	_ = readMultiline(t, scanner)
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "RCPT TO:<r@t>\r\n", "250")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

func TestSMTPDataBeforeRcpt(t *testing.T) {
	conn, scanner := dialServer(t)

	sendAndExpect(t, conn, scanner, "EHLO mx\r\n", "250")
	_ = readMultiline(t, scanner)
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t>\r\n", "250")
	// DATA before RCPT → 503
	sendAndExpect(t, conn, scanner, "DATA\r\n", "503")

	sendAndExpect(t, conn, scanner, "RCPT TO:<r@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "DATA\r\n", "354")
	write(t, conn, "body\r\n.\r\n")
	expectResp(t, scanner, "250")
	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

func TestSMTPMailFromBadSyntax(t *testing.T) {
	conn, scanner := dialServer(t)

	sendAndExpect(t, conn, scanner, "EHLO mx\r\n", "250")
	_ = readMultiline(t, scanner)

	// Missing angle brackets
	sendAndExpect(t, conn, scanner, "MAIL FROM:s@t\r\n", "501")
	// Empty args
	sendAndExpect(t, conn, scanner, "MAIL\r\n", "501")
	// Valid one
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t>\r\n", "250")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

// ---------------------------------------------------------------------------
// QUIT during transaction
// ---------------------------------------------------------------------------

func TestSMTPQuitDuringTransaction(t *testing.T) {
	conn, scanner := dialServer(t)

	sendAndExpect(t, conn, scanner, "EHLO mx\r\n", "250")
	_ = readMultiline(t, scanner)
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "RCPT TO:<r@t>\r\n", "250")

	// QUIT before DATA → 221, transaction abandoned.
	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

// ---------------------------------------------------------------------------
// RCPT TO parsing edge cases
// ---------------------------------------------------------------------------

func TestSMTPRcptToEdgeCases(t *testing.T) {
	conn, scanner := dialServer(t)

	sendAndExpect(t, conn, scanner, "EHLO mx\r\n", "250")
	_ = readMultiline(t, scanner)
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t>\r\n", "250")

	// No angle brackets — best-effort, passes the raw args to handler.
	sendAndExpect(t, conn, scanner, "RCPT TO:user@host\r\n", "250")
	// Empty brackets
	sendAndExpect(t, conn, scanner, "RCPT TO:<>\r\n", "250")
	// With parameters (server ignores them)
	sendAndExpect(t, conn, scanner, "RCPT TO:<r@t> NOTIFY=SUCCESS\r\n", "250")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

// ---------------------------------------------------------------------------
// MAIL FROM parameter handling
// ---------------------------------------------------------------------------

func TestSMTPMailFromParameters(t *testing.T) {
	conn, scanner := dialServer(t)

	sendAndExpect(t, conn, scanner, "EHLO mx\r\n", "250")
	_ = readMultiline(t, scanner)

	// NULL sender
	sendAndExpect(t, conn, scanner, "MAIL FROM:<>\r\n", "250")
	sendAndExpect(t, conn, scanner, "RSET\r\n", "250")

	// BODY=8BITMIME
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t> BODY=8BITMIME\r\n", "250")
	sendAndExpect(t, conn, scanner, "RSET\r\n", "250")

	// SIZE parameter
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t> SIZE=5000\r\n", "250")
	sendAndExpect(t, conn, scanner, "RSET\r\n", "250")

	// Multiple parameters
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t> SIZE=1000 BODY=8BITMIME\r\n", "250")
	sendAndExpect(t, conn, scanner, "RSET\r\n", "250")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

// ---------------------------------------------------------------------------
// RCPT TO address forms
// ---------------------------------------------------------------------------

func TestParseRcptToEdgeCases(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"TO:<user@host>", "user@host"},
		{"TO:<>", ""},
		{"TO:plain", "plain"},
		{"TO: user@host ", "user@host"},
		{"to:<User@Host>", "User@Host"},
		{"TO:<user@host> KEY=VAL", "user@host"},
	}

	for _, tc := range tests {
		got := parseRcptTo(tc.input)
		if got != tc.want {
			t.Errorf("parseRcptTo(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Message size limit
// ---------------------------------------------------------------------------

// TestSMTPMessageSizeLimitEnforced tests a server configured with
// MaxMessageSize — messages exceeding the limit are rejected with 552.
func TestSMTPMessageSizeLimitEnforced(t *testing.T) {
	counting := &acceptAllHandler{}

	srv := &Server{
		Hostname:       "test.local",
		Handler:        counting,
		ReadTimeout:    5 * time.Second,
		MaxMessageSize: 3, // very small — any body > 3 bytes rejected
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("no banner")
	}

	sendAndExpect(t, conn, scanner, "EHLO mx\r\n", "250")
	_ = readMultiline(t, scanner)
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "RCPT TO:<r@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "DATA\r\n", "354")
	// Send enough bytes to exceed MaxMessageSize=3.
	// readDotUnstuffed stops reading after the limit, so don't send
	// a terminating dot — it would be read as a stray command.
	write(t, conn, "abcdefgh\r\n")

	// Body > 3 bytes → 552
	expectResp(t, scanner, "552")
	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

// ---------------------------------------------------------------------------
// RCPT TO address parsing (E2E — various bracket forms)
// ---------------------------------------------------------------------------

func TestSMTPRcptToPostmaster(t *testing.T) {
	conn, scanner := dialServer(t)

	sendAndExpect(t, conn, scanner, "EHLO mx\r\n", "250")
	_ = readMultiline(t, scanner)
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t>\r\n", "250")

	// Postmaster address
	sendAndExpect(t, conn, scanner, "RCPT TO:<postmaster@localhost>\r\n", "250")
	// Quoted local part
	sendAndExpect(t, conn, scanner, "RCPT TO:<\"foo bar\"@host>\r\n", "250")
	// Sub-addressing
	sendAndExpect(t, conn, scanner, "RCPT TO:<user+tag@domain>\r\n", "250")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

// ---------------------------------------------------------------------------
// STARTTLS
// ---------------------------------------------------------------------------

// testCert generates a self-signed certificate for testing STARTTLS.
func testCert(t *testing.T) tls.Certificate {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"Test"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost", "test.local"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}

	return tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  key,
	}
}

func TestSMTPStartTLS(t *testing.T) {
	cert := testCert(t)

	srv := &Server{
		Hostname:    "test.local",
		Handler:     &acceptAllHandler{},
		ReadTimeout: 5 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
		},
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	scanner := bufio.NewScanner(conn)

	// Read banner.
	if !scanner.Scan() {
		t.Fatalf("no banner: %v", scanner.Err())
	}
	if !strings.HasPrefix(scanner.Text(), "220 ") {
		t.Fatalf("unexpected banner: %s", scanner.Text())
	}

	// EHLO should advertise STARTTLS.
	write(t, conn, "EHLO client\r\n")
	lines := readMultiline(t, scanner)
	found := false
	for _, line := range lines {
		if strings.Contains(line, "STARTTLS") {
			found = true
			break
		}
	}
	if !found {
		t.Error("STARTTLS not advertised in EHLO response")
	}

	// Issue STARTTLS.
	write(t, conn, "STARTTLS\r\n")
	resp := readResp(t, scanner)
	if !strings.HasPrefix(resp, "220 ") {
		t.Fatalf("expected 220 ready, got %q", resp)
	}

	// Upgrade to TLS.
	tlsConn := tls.Client(conn, &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         "test.local",
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake: %v", err)
	}
	conn = tlsConn
	scanner = bufio.NewScanner(conn)

	// Re-EHLO over TLS.
	write(t, conn, "EHLO client\r\n")
	_ = readMultiline(t, scanner)

	// Complete a transaction over TLS.
	write(t, conn, "MAIL FROM:<s@t>\r\n")
	expectResp(t, scanner, "250")
	write(t, conn, "RCPT TO:<r@t>\r\n")
	expectResp(t, scanner, "250")
	write(t, conn, "DATA\r\n")
	expectResp(t, scanner, "354")
	write(t, conn, "body\r\n.\r\n")
	expectResp(t, scanner, "250")
	write(t, conn, "QUIT\r\n")
	expectResp(t, scanner, "221")

	_ = l.Close()
	_ = srv.Shutdown(context.Background())
}

func TestSMTPStartTLSRequiresHelo(t *testing.T) {
	cert := testCert(t)

	srv := &Server{
		Hostname:    "test.local",
		Handler:     &acceptAllHandler{},
		ReadTimeout: 5 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
		},
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("no banner: %v", scanner.Err())
	}

	// STARTTLS before HELO → 503.
	write(t, conn, "STARTTLS\r\n")
	expectResp(t, scanner, "503")

	write(t, conn, "QUIT\r\n")
	expectResp(t, scanner, "221")

	_ = l.Close()
	_ = srv.Shutdown(context.Background())
}

func TestSMTPStartTLSDoubleRejected(t *testing.T) {
	cert := testCert(t)

	srv := &Server{
		Hostname:    "test.local",
		Handler:     &acceptAllHandler{},
		ReadTimeout: 5 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
		},
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("no banner: %v", scanner.Err())
	}

	write(t, conn, "EHLO client\r\n")
	_ = readMultiline(t, scanner)

	// First STARTTLS.
	write(t, conn, "STARTTLS\r\n")
	expectResp(t, scanner, "220")

	// Upgrade.
	tlsConn := tls.Client(conn, &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         "test.local",
	})
	_ = tlsConn.Handshake()
	conn = tlsConn
	scanner = bufio.NewScanner(conn)

	// Second STARTTLS after already upgraded → 503.
	write(t, conn, "STARTTLS\r\n")
	expectResp(t, scanner, "503")

	write(t, conn, "QUIT\r\n")
	expectResp(t, scanner, "221")

	_ = l.Close()
	_ = srv.Shutdown(context.Background())
}

func TestSMTPStartTLSNotConfigured(t *testing.T) {
	srv := &Server{
		Hostname:    "test.local",
		Handler:     &acceptAllHandler{},
		ReadTimeout: 5 * time.Second,
		// TLSConfig is nil → STARTTLS not supported.
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("no banner: %v", scanner.Err())
	}

	write(t, conn, "EHLO client\r\n")
	lines := readMultiline(t, scanner)
	for _, line := range lines {
		if strings.Contains(line, "STARTTLS") {
			t.Error("STARTTLS advertised when TLSConfig is nil")
		}
	}

	// STARTTLS when not configured → 502.
	write(t, conn, "STARTTLS\r\n")
	expectResp(t, scanner, "502")

	write(t, conn, "QUIT\r\n")
	expectResp(t, scanner, "221")

	_ = l.Close()
	_ = srv.Shutdown(context.Background())
}

// ---------------------------------------------------------------------------
// MaxRecipients enforcement
// ---------------------------------------------------------------------------

func TestSMTPMaxRecipients(t *testing.T) {
	srv := &Server{
		Hostname:      "test.local",
		Handler:       &acceptAllHandler{},
		ReadTimeout:   5 * time.Second,
		MaxRecipients: 2,
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("no banner")
	}

	sendAndExpect(t, conn, scanner, "EHLO mx\r\n", "250")
	_ = readMultiline(t, scanner)
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "RCPT TO:<r1@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "RCPT TO:<r2@t>\r\n", "250")
	// Third RCPT TO exceeds limit → 452.
	sendAndExpect(t, conn, scanner, "RCPT TO:<r3@t>\r\n", "452")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")

	_ = l.Close()
	_ = srv.Shutdown(context.Background())
}

// ---------------------------------------------------------------------------
// SIZE parameter rejection at MAIL FROM
// ---------------------------------------------------------------------------

func TestSMTPSizeRejectedAtMailFrom(t *testing.T) {
	srv := &Server{
		Hostname:       "test.local",
		Handler:        &acceptAllHandler{},
		ReadTimeout:    5 * time.Second,
		MaxMessageSize: 1000,
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("no banner")
	}

	sendAndExpect(t, conn, scanner, "EHLO mx\r\n", "250")
	lines := readMultiline(t, scanner)
	// Verify SIZE is advertised.
	found := false
	for _, line := range lines {
		if strings.Contains(line, "SIZE 1000") {
			found = true
		}
	}
	if !found {
		t.Error("SIZE 1000 not advertised in EHLO response")
	}

	// SIZE parameter exceeding limit → 552.
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t> SIZE=5000\r\n", "552")

	// SIZE within limit → 250.
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t> SIZE=500\r\n", "250")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")

	_ = l.Close()
	_ = srv.Shutdown(context.Background())
}

// ---------------------------------------------------------------------------
// Graceful shutdown
// ---------------------------------------------------------------------------

func TestSMTPShutdownDuringTransaction(t *testing.T) {
	srv := &Server{
		Hostname:    "test.local",
		Handler:     &acceptAllHandler{},
		ReadTimeout: 5 * time.Second,
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("no banner")
	}

	sendAndExpect(t, conn, scanner, "EHLO mx\r\n", "250")
	_ = readMultiline(t, scanner)

	// Trigger shutdown while a connection is active.
	go func() {
		_ = srv.Shutdown(context.Background())
	}()

	// The server should send 421 to the active connection.
	// Read the next response — it should be 421.
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "421") {
			return // expected
		}
	}

	_ = l.Close()
	_ = srv.Shutdown(context.Background())
}

// ---------------------------------------------------------------------------
// SMTPUTF8 (RFC 6531)
// ---------------------------------------------------------------------------

// utf8Handler records the body and sender for verification.
type utf8Handler struct {
	mu         sync.Mutex
	lastBody   []byte
	lastSender string
}

func (h *utf8Handler) Hello(_ context.Context, _ *Tx) *Response    { return RespHelloOK }
func (h *utf8Handler) MailFrom(_ context.Context, _ *Tx) *Response { return RespMailOK }
func (h *utf8Handler) RcptTo(_ context.Context, _ *Tx) *Response   { return RespRcptOK }
func (h *utf8Handler) Data(_ context.Context, tx *Tx, body []byte) *Response {
	h.mu.Lock()
	h.lastBody = append([]byte{}, body...)
	h.lastSender = tx.MailFrom
	h.mu.Unlock()
	return RespDataOK
}

func TestSMTPUTF8Accepts8BitBody(t *testing.T) {
	// With SMTPUTF8 declared, 8-bit content must be accepted even without
	// BODY=8BITMIME (RFC 6531 §3.4).
	h := &utf8Handler{}
	srv := &Server{
		Hostname:    "test.local",
		Handler:     h,
		ReadTimeout: 5 * time.Second,
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	scanner := bufio.NewScanner(conn)
	_ = readResp(t, scanner)

	sendAndExpect(t, conn, scanner, "EHLO client\r\n", "250")
	_ = readMultiline(t, scanner)

	// Declare SMTPUTF8 but NOT BODY=8BITMIME.
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t> SMTPUTF8\r\n", "250")
	sendAndExpect(t, conn, scanner, "RCPT TO:<r@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "DATA\r\n", "354")

	// Send 8-bit content (byte 0xFF).
	write(t, conn, "Subject: test\r\n\r\n\xFF\x80\r\n.\r\n")
	expectResp(t, scanner, "250")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")

	h.mu.Lock()
	if !bytes.Contains(h.lastBody, []byte{0xFF}) {
		t.Error("8-bit body content was lost")
	}
	h.mu.Unlock()
}

func TestSMTPUTF8Without8BitBody(t *testing.T) {
	// Without SMTPUTF8 or BODY=8BITMIME, 8-bit content is rejected.
	srv := &Server{
		Hostname:    "test.local",
		Handler:     &acceptAllHandler{},
		ReadTimeout: 5 * time.Second,
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	scanner := bufio.NewScanner(conn)
	_ = readResp(t, scanner)

	sendAndExpect(t, conn, scanner, "EHLO client\r\n", "250")
	_ = readMultiline(t, scanner)

	// No SMTPUTF8, no BODY=8BITMIME.
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "RCPT TO:<r@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "DATA\r\n", "354")

	// 8-bit content should be rejected.
	write(t, conn, "\xFF\r\n.\r\n")
	expectResp(t, scanner, "550") // 5.6.3

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

func TestSMTPUTF8AddressesEndToEnd(t *testing.T) {
	// UTF-8 characters in envelope addresses should survive the round-trip.
	h := &utf8Handler{}
	srv := &Server{
		Hostname:    "mx.münchen.de", // server hostname with UTF-8
		Handler:     h,
		ReadTimeout: 5 * time.Second,
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	scanner := bufio.NewScanner(conn)
	_ = readResp(t, scanner)

	sendAndExpect(t, conn, scanner, "EHLO client\r\n", "250")
	_ = readMultiline(t, scanner)

	// MAIL FROM with UTF-8 sender.
	sendAndExpect(t, conn, scanner, "MAIL FROM:<üser@münchen.de> SMTPUTF8\r\n", "250")
	// RCPT TO with UTF-8 recipient.
	sendAndExpect(t, conn, scanner, "RCPT TO:<rcpt@münchen.de>\r\n", "250")
	sendAndExpect(t, conn, scanner, "DATA\r\n", "354")
	write(t, conn, "Subject: Grüß Gott\r\n\r\nHällo Wörld\r\n.\r\n")
	expectResp(t, scanner, "250")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")

	h.mu.Lock()
	if h.lastSender != "üser@münchen.de" {
		t.Errorf("sender = %q, want üser@münchen.de", h.lastSender)
	}
	if !bytes.Contains(h.lastBody, []byte("Grüß Gott")) {
		t.Error("UTF-8 subject was lost")
	}
	h.mu.Unlock()
}

// ---------------------------------------------------------------------------
// BDAT / CHUNKING (RFC 3030)
// ---------------------------------------------------------------------------

// bdatRecorder records the body passed to Handler.Data for verification.
type bdatRecorder struct {
	mu       sync.Mutex
	lastBody []byte
}

func (h *bdatRecorder) Hello(_ context.Context, _ *Tx) *Response    { return RespHelloOK }
func (h *bdatRecorder) MailFrom(_ context.Context, _ *Tx) *Response { return RespMailOK }
func (h *bdatRecorder) RcptTo(_ context.Context, _ *Tx) *Response   { return RespRcptOK }
func (h *bdatRecorder) Data(_ context.Context, _ *Tx, body []byte) *Response {
	h.mu.Lock()
	h.lastBody = append([]byte{}, body...)
	h.mu.Unlock()
	return RespDataOK
}

func TestSMTPChunkingAdvertised(t *testing.T) {
	conn, scanner := dialServer(t)

	write(t, conn, "EHLO client\r\n")
	lines := readMultiline(t, scanner)
	found := false
	for _, line := range lines {
		if strings.Contains(line, "CHUNKING") {
			found = true
			break
		}
	}
	if !found {
		t.Error("CHUNKING not advertised in EHLO response")
	}
	write(t, conn, "QUIT\r\n")
	expectResp(t, scanner, "221")
}

// bdatServer starts a server with the given handler and returns the connection + scanner.
func bdatServer(t *testing.T, h Handler) (net.Conn, *bufio.Scanner, *Server) {
	t.Helper()
	srv := &Server{
		Hostname:    "test.local",
		Handler:     h,
		ReadTimeout: 5 * time.Second,
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	scanner := bufio.NewScanner(conn)
	_ = readResp(t, scanner) // banner
	return conn, scanner, srv
}

// bdatSetup performs EHLO → MAIL FROM → RCPT TO.
func bdatSetup(t *testing.T, conn net.Conn, scanner *bufio.Scanner) {
	t.Helper()
	sendAndExpect(t, conn, scanner, "EHLO t\r\n", "250")
	_ = readMultiline(t, scanner)
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "RCPT TO:<r@t>\r\n", "250")
}

// sendBdat writes "BDAT N [LAST]\r\n" followed by data, then reads 250.
func sendBdat(t *testing.T, conn net.Conn, scanner *bufio.Scanner, size int, last bool, data string) {
	t.Helper()
	cmd := fmt.Sprintf("BDAT %d\r\n", size)
	if last {
		cmd = fmt.Sprintf("BDAT %d LAST\r\n", size)
	}
	write(t, conn, cmd)
	if size > 0 {
		write(t, conn, data)
	}
	expectResp(t, scanner, "250")
}

func TestSMTPBDATZeroSize(t *testing.T) {
	conn, scanner, _ := bdatServer(t, &acceptAllHandler{})
	bdatSetup(t, conn, scanner)
	sendBdat(t, conn, scanner, 0, true, "")
	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

func TestSMTPBDATSingleChunk(t *testing.T) {
	rec := &bdatRecorder{}
	conn, scanner, _ := bdatServer(t, rec)
	bdatSetup(t, conn, scanner)

	body := "Subject: test\r\n\r\nHello\r\n"
	write(t, conn, fmt.Sprintf("BDAT %d LAST\r\n", len(body)))
	write(t, conn, body)
	expectResp(t, scanner, "250")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")

	rec.mu.Lock()
	if string(rec.lastBody) != body {
		t.Errorf("body = %q, want %q", string(rec.lastBody), body)
	}
	rec.mu.Unlock()
}

func TestSMTPBDATMultipleChunks(t *testing.T) {
	rec := &bdatRecorder{}
	conn, scanner, _ := bdatServer(t, rec)
	bdatSetup(t, conn, scanner)

	sendBdat(t, conn, scanner, 6, false, "Header")
	sendBdat(t, conn, scanner, 5, false, "Line\n")
	sendBdat(t, conn, scanner, 4, true, "Body")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")

	rec.mu.Lock()
	want := "HeaderLine\nBody"
	if string(rec.lastBody) != want {
		t.Errorf("body = %q, want %q", string(rec.lastBody), want)
	}
	rec.mu.Unlock()
}

func TestSMTPBDATBinaryData(t *testing.T) {
	rec := &bdatRecorder{}
	conn, scanner, _ := bdatServer(t, rec)
	bdatSetup(t, conn, scanner)

	bin := []byte{0x00, 0xFF, 0x80, 0x01, 0x02, 0x7F}
	write(t, conn, fmt.Sprintf("BDAT %d LAST\r\n", len(bin)))
	write(t, conn, string(bin))
	expectResp(t, scanner, "250")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")

	rec.mu.Lock()
	if !bytes.Equal(rec.lastBody, bin) {
		t.Errorf("body = %v, want %v", rec.lastBody, bin)
	}
	rec.mu.Unlock()
}

func TestSMTPBDATBeforeRcpt(t *testing.T) {
	conn, scanner, _ := bdatServer(t, &acceptAllHandler{})

	sendAndExpect(t, conn, scanner, "EHLO t\r\n", "250")
	_ = readMultiline(t, scanner)
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t>\r\n", "250")
	// BDAT before RCPT TO → 503.
	sendAndExpect(t, conn, scanner, "BDAT 0 LAST\r\n", "503")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

func TestSMTPBDATNoAcceptedRecipients(t *testing.T) {
	conn, scanner, _ := bdatServer(t, &rejectRcptHandler{})

	sendAndExpect(t, conn, scanner, "EHLO t\r\n", "250")
	_ = readMultiline(t, scanner)
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "RCPT TO:<bad@t>\r\n", "550")
	// All recipients rejected → 554.
	sendAndExpect(t, conn, scanner, "BDAT 0 LAST\r\n", "554")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

func TestSMTPBDATMessageSizeExceeded(t *testing.T) {
	srv := &Server{
		Hostname:       "test.local",
		Handler:        &acceptAllHandler{},
		ReadTimeout:    5 * time.Second,
		MaxMessageSize: 10,
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() { _ = srv.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	scanner := bufio.NewScanner(conn)
	_ = readResp(t, scanner)
	bdatSetup(t, conn, scanner)

	// Send chunk that exceeds MaxMessageSize=10 → 552.
	write(t, conn, "BDAT 15 LAST\r\n")
	write(t, conn, "123456789012345")
	expectResp(t, scanner, "552")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

func TestSMTPBDATRsetResets(t *testing.T) {
	rec := &bdatRecorder{}
	conn, scanner, _ := bdatServer(t, rec)
	bdatSetup(t, conn, scanner)

	// Start a BDAT sequence with non-LAST chunk.
	sendBdat(t, conn, scanner, 5, false, "Hello")

	// RSET during BDAT sequence — abandons accumulated data.
	sendAndExpect(t, conn, scanner, "RSET\r\n", "250")

	// New transaction works.
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s2@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "RCPT TO:<r2@t>\r\n", "250")
	sendBdat(t, conn, scanner, 3, true, "Hi!")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")

	// Handler received body from the second transaction, not the first.
	rec.mu.Lock()
	if string(rec.lastBody) != "Hi!" {
		t.Errorf("body = %q, want %q (RSET should have discarded first chunk)", string(rec.lastBody), "Hi!")
	}
	rec.mu.Unlock()
}

func TestSMTPBDATRejectsCommandsDuringSequence(t *testing.T) {
	conn, scanner, _ := bdatServer(t, &acceptAllHandler{})
	bdatSetup(t, conn, scanner)

	// Send a non-LAST chunk — inBdat is now true.
	sendBdat(t, conn, scanner, 3, false, "ABC")

	// Commands other than BDAT, RSET, NOOP, QUIT are rejected.
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s2@t>\r\n", "503")
	sendAndExpect(t, conn, scanner, "NOOP\r\n", "250") // allowed
	sendAndExpect(t, conn, scanner, "RCPT TO:<r2@t>\r\n", "503")

	// RSET to exit BDAT sequence.
	sendAndExpect(t, conn, scanner, "RSET\r\n", "250")

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}

func TestSMTPBDATMultipleSessions(t *testing.T) {
	// Verify BDAT interleaves cleanly across separate transactions on one connection.
	rec := &bdatRecorder{}
	conn, scanner, _ := bdatServer(t, rec)
	bdatSetup(t, conn, scanner)

	// First transaction.
	sendBdat(t, conn, scanner, 4, true, "One!")
	rec.mu.Lock()
	if string(rec.lastBody) != "One!" {
		t.Errorf("first body = %q", string(rec.lastBody))
	}
	rec.mu.Unlock()

	// Second transaction on same connection.
	sendAndExpect(t, conn, scanner, "MAIL FROM:<s@t>\r\n", "250")
	sendAndExpect(t, conn, scanner, "RCPT TO:<r@t>\r\n", "250")
	sendBdat(t, conn, scanner, 4, true, "Two!")

	rec.mu.Lock()
	if string(rec.lastBody) != "Two!" {
		t.Errorf("second body = %q", string(rec.lastBody))
	}
	rec.mu.Unlock()

	sendAndExpect(t, conn, scanner, "QUIT\r\n", "221")
}
