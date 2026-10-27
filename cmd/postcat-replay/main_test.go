package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
	"time"
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
