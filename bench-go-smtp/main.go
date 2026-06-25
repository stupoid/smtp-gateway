// go-smtp-server mirrors test-server but uses the go-smtp library,
// so we can benchmark smtp-gateway's custom SMTP impl against a
// widely-used reference.
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/emersion/go-smtp"
)

type postcatBackend struct {
	postcatDir string
}

func (b *postcatBackend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	return &postcatSession{postcatDir: b.postcatDir}, nil
}

type postcatSession struct {
	postcatDir string
	from       string
	to         []string
	data       []byte
}

func (s *postcatSession) AuthPlain(_, _ string) error {
	return nil
}

func (s *postcatSession) Mail(from string, _ *smtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *postcatSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.to = append(s.to, to)
	return nil
}

func (s *postcatSession) Data(r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.data = body

	// Write postcat file — same format as smtp-gateway's postcat.Write.
	t := time.Now()
	fname := fmt.Sprintf("%d-%d.eml", t.Unix(), t.Nanosecond())
	f, err := os.CreateTemp(s.postcatDir, "."+fname+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()

	// Format null sender as <> to match smtp-gateway's postcat format.
	sender := s.from
	if sender == "" || sender == "<>" {
		sender = "<>"
	}
	fmt.Fprintf(f, "S %s\n", sender)
	for _, rcpt := range s.to {
		fmt.Fprintf(f, "R %s\n", rcpt)
	}
	fmt.Fprintf(f, "T %s\n", t.Format(time.RFC3339))
	f.WriteString("\n")
	if _, err := f.Write(s.data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	final := filepath.Join(s.postcatDir, fname)
	if err := os.Rename(f.Name(), final); err != nil {
		return err
	}
	return nil
}

func (s *postcatSession) Reset() {
	s.from = ""
	s.to = nil
	s.data = nil
}

func (s *postcatSession) Logout() error {
	return nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: go-smtp-server <listen-addr> <postcat-dir>\n")
		os.Exit(1)
	}
	addr := os.Args[1]
	postcatDir := os.Args[2]

	if err := os.MkdirAll(postcatDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir %s: %v\n", postcatDir, err)
		os.Exit(1)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = ln.Close() }()

	srv := smtp.NewServer(&postcatBackend{postcatDir: postcatDir})
	srv.Addr = addr
	srv.Domain = "test.local"
	srv.ReadTimeout = 30 * time.Second
	srv.WriteTimeout = 30 * time.Second
	srv.MaxMessageBytes = 50 * 1024 * 1024
	srv.MaxRecipients = 50
	srv.AllowInsecureAuth = true

	fmt.Printf("LISTENING %s\n", ln.Addr().String())

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		fmt.Fprintf(os.Stderr, "shutting down...\n")
		_ = ln.Close()
	}()

	if err := srv.Serve(ln); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}
