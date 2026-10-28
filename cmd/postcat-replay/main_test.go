package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stupoid/smtp-gateway/internal/postcat"
)

func TestFormatSender(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"normal", "s@t.com", "<s@t.com>"},
		{"empty is null sender", "", "<>"},
		{"literal null sender", "<>", "<>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatSender(tt.in); got != tt.want {
				t.Errorf("formatSender(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestExpect(t *testing.T) {
	t.Run("multi-line", func(t *testing.T) {
		r := bufio.NewReader(strings.NewReader("250-first\r\n250 second\r\n"))
		if err := expect(r, 250); err != nil {
			t.Fatalf("expect: %v", err)
		}
	})
	t.Run("wrong code", func(t *testing.T) {
		r := bufio.NewReader(strings.NewReader("550 no\r\n"))
		if err := expect(r, 250); err == nil {
			t.Fatal("expected an error for 550")
		}
	})
	t.Run("garbage", func(t *testing.T) {
		r := bufio.NewReader(strings.NewReader("garbage\r\n"))
		if err := expect(r, 250); err == nil {
			t.Fatal("expected an error for a non-numeric reply")
		}
	})
}

// captureConn records writes.  It is a net.Conn so sendBody can use it
// without a socket.
type captureConn struct {
	bytes.Buffer
}

func (c *captureConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *captureConn) Close() error                     { return nil }
func (c *captureConn) LocalAddr() net.Addr              { return nil }
func (c *captureConn) RemoteAddr() net.Addr             { return nil }
func (c *captureConn) SetDeadline(time.Time) error      { return nil }
func (c *captureConn) SetReadDeadline(time.Time) error  { return nil }
func (c *captureConn) SetWriteDeadline(time.Time) error { return nil }

func TestSendBody(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want string
	}{
		{
			name: "dot stuffing and blank line",
			body: []byte("line1\r\n.leading\r\n\r\nlast"),
			want: "line1\r\n..leading\r\n\r\nlast\r\n.\r\n",
		},
		{
			name: "trailing newline",
			body: []byte("a\r\n"),
			want: "a\r\n.\r\n",
		},
		{
			name: "bare LF",
			body: []byte("a\nb\n"),
			want: "a\r\nb\r\n.\r\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &captureConn{}
			if err := sendBody(c, tt.body); err != nil {
				t.Fatalf("sendBody: %v", err)
			}
			if got := c.String(); got != tt.want {
				t.Errorf("sendBody = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut); code != 1 {
		t.Errorf("run code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "usage: postcat-replay") {
		t.Errorf("stderr = %q, want usage", errOut.String())
	}
}

func TestRunReplaysMessage(t *testing.T) {
	dir := t.TempDir()
	path, err := postcat.Write(dir, "s@t.com", []string{"r@t.com"}, []byte("Subject: test\r\n\r\nhello\r\n"))
	if err != nil {
		t.Fatalf("postcat.Write: %v", err)
	}

	addr, stop := fakeSMTPServer(t)
	defer stop()

	var out, errOut bytes.Buffer
	if code := run([]string{"-addr", addr, path}, &out, &errOut); code != 0 {
		t.Fatalf("run code = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "OK   "+path) {
		t.Errorf("stdout = %q, want an OK line", out.String())
	}
}

// fakeSMTPServer accepts one message and replies to the command
// sequence that replay uses.  It fails the test only on setup.
func fakeSMTPServer(t *testing.T) (string, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		r := bufio.NewReader(conn)
		_, _ = io.WriteString(conn, "220 test\r\n")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"):
				_, _ = io.WriteString(conn, "250 test\r\n")
			case strings.HasPrefix(cmd, "MAIL"):
				_, _ = io.WriteString(conn, "250 2.1.0 OK\r\n")
			case strings.HasPrefix(cmd, "RCPT"):
				_, _ = io.WriteString(conn, "250 2.1.5 OK\r\n")
			case cmd == "DATA":
				_, _ = io.WriteString(conn, "354 go ahead\r\n")
				for {
					bodyLine, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if bodyLine == ".\r\n" {
						break
					}
				}
				_, _ = io.WriteString(conn, "250 2.0.0 OK\r\n")
			case cmd == "QUIT":
				_, _ = io.WriteString(conn, "221 bye\r\n")
				return
			}
		}
	}()
	return l.Addr().String(), func() {
		_ = l.Close()
		<-done
	}
}
