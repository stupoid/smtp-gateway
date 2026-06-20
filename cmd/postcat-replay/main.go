// postcat-replay replays postcat-format message files through an SMTP
// server.  It parses each file, connects to the target server, and
// replays the full envelope + body as a normal SMTP transaction.
//
// Usage:
//
//	postcat-replay [flags] <file.eml>...
//
// Flags:
//
//	-addr  SMTP server address (default ":2525")
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/stupoid/smtp-gateway/internal/postcat"
)

func main() {
	addr := flag.String("addr", ":2525", "SMTP server address")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "usage: postcat-replay [flags] <file.eml>...\n")
		os.Exit(1)
	}

	failed := 0
	for _, path := range flag.Args() {
		if err := replay(*addr, path); err != nil {
			fmt.Fprintf(os.Stderr, "FAIL %s: %v\n", path, err)
			failed++
		} else {
			fmt.Printf("OK   %s\n", path)
		}
	}
	if failed > 0 {
		os.Exit(1)
	}
}

func replay(addr, path string) error {
	msg, err := postcat.Parse(path)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}

	if len(msg.Recipients) == 0 {
		return fmt.Errorf("no recipients in postcat file")
	}
	if len(msg.RawMessage) == 0 {
		return fmt.Errorf("empty body in postcat file")
	}

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	r := bufio.NewReader(conn)

	// 1. Read greeting.
	if err := expect(r, 220); err != nil {
		return fmt.Errorf("greeting: %w", err)
	}

	// 2. EHLO.
	if _, err := fmt.Fprint(conn, "EHLO replay.local\r\n"); err != nil {
		return fmt.Errorf("write EHLO: %w", err)
	}
	if err := expect(r, 250); err != nil {
		return fmt.Errorf("EHLO: %w", err)
	}

	// 3. MAIL FROM.
	from := formatSender(msg.Sender)
	if _, err := fmt.Fprintf(conn, "MAIL FROM:%s\r\n", from); err != nil {
		return fmt.Errorf("write MAIL FROM: %w", err)
	}
	if err := expect(r, 250); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}

	// 4. RCPT TO (one per recipient).
	for _, rcpt := range msg.Recipients {
		if _, err := fmt.Fprintf(conn, "RCPT TO:<%s>\r\n", rcpt); err != nil {
			return fmt.Errorf("write RCPT TO: %w", err)
		}
		if err := expect(r, 250); err != nil {
			return fmt.Errorf("RCPT TO <%s>: %w", rcpt, err)
		}
	}

	// 5. DATA.
	if _, err := fmt.Fprint(conn, "DATA\r\n"); err != nil {
		return fmt.Errorf("write DATA: %w", err)
	}
	if err := expect(r, 354); err != nil {
		return fmt.Errorf("DATA: %w", err)
	}

	// 6. Send dot-stuffed body + terminating dot.
	if err := sendBody(conn, msg.RawMessage); err != nil {
		return fmt.Errorf("send body: %w", err)
	}

	// 7. Read DATA response.
	if err := expect(r, 250); err != nil {
		return fmt.Errorf("DATA response: %w", err)
	}

	// 8. QUIT.
	if _, err := fmt.Fprint(conn, "QUIT\r\n"); err != nil {
		return fmt.Errorf("write QUIT: %w", err)
	}
	_ = expect(r, 221) // best-effort

	return nil
}

// formatSender returns the SMTP reverse-path for MAIL FROM.
// An empty sender (null sender) becomes "<>".
func formatSender(sender string) string {
	if sender == "" || sender == "<>" {
		return "<>"
	}
	return "<" + sender + ">"
}

// sendBody writes the raw message to conn with dot-stuffing and a
// terminating dot line.
func sendBody(conn net.Conn, body []byte) error {
	lines := strings.Split(string(body), "\r\n")
	// If body ends with \r\n, strings.Split produces an empty trailing
	// element that we should not emit as a line.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	for _, line := range lines {
		if strings.HasPrefix(line, ".") {
			line = "." + line
		}
		if _, err := io.WriteString(conn, line+"\r\n"); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(conn, ".\r\n"); err != nil {
		return err
	}
	return nil
}

// expect reads SMTP response lines from r until it sees a line that
// starts with code followed by a space (not a dash).  Returns an error
// if any response code does not match wantCode.
func expect(r *bufio.Reader, wantCode int) error {
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")

		code := 0
		if _, scanErr := fmt.Sscanf(line, "%d", &code); scanErr != nil || code < 200 {
			return fmt.Errorf("bad response: %s", line)
		}

		isLast := len(line) > 3 && line[3] == ' '
		if code != wantCode {
			return fmt.Errorf("unexpected %d (want %d): %s", code, wantCode, line)
		}
		if isLast {
			return nil
		}
		// Multi-line continuation ("250-...") — keep reading.
	}
}
