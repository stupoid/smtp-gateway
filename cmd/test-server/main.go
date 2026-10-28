// test-server is a minimal SMTP server for end-to-end testing.
// It accepts all mail and writes it to postcat files.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	smtpgateway "github.com/stupoid/smtp-gateway"
)

type acceptAllHandler struct{}

func (h *acceptAllHandler) Hello(_ context.Context, _ *smtpgateway.Tx) *smtpgateway.Response {
	return smtpgateway.RespHelloOK
}
func (h *acceptAllHandler) MailFrom(_ context.Context, _ *smtpgateway.Tx) *smtpgateway.Response {
	return smtpgateway.RespMailOK
}
func (h *acceptAllHandler) RcptTo(_ context.Context, _ *smtpgateway.Tx) *smtpgateway.Response {
	return smtpgateway.RespRcptOK
}
func (h *acceptAllHandler) Data(_ context.Context, _ *smtpgateway.Tx, _ []byte) *smtpgateway.Response {
	return smtpgateway.RespDataOK
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("test-server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tlsCert := fs.String("tls-cert", "", "TLS certificate file (enables STARTTLS)")
	tlsKey := fs.String("tls-key", "", "TLS key file (enables STARTTLS)")
	requireTLS := fs.Bool("require-tls", false, "reject plaintext MAIL with 530")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if fs.NArg() < 2 {
		return errors.New("usage: test-server [-tls-cert cert.pem -tls-key key.pem] [-require-tls] <listen-addr> <postcat-dir>")
	}
	addr := fs.Arg(0)
	postcatDir := fs.Arg(1)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer func() { _ = ln.Close() }()

	srv := smtpgateway.NewServer(
		&acceptAllHandler{},
		smtpgateway.WithHostname("test.local"),
		smtpgateway.WithPostcatDir(postcatDir),
		smtpgateway.WithReadTimeout(30*time.Second),
		smtpgateway.WithWriteTimeout(30*time.Second),
		smtpgateway.WithIdleTimeout(60*time.Second),
	)

	if *tlsCert != "" && *tlsKey != "" {
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			return fmt.Errorf("load TLS key pair: %w", err)
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	}
	srv.RequireTLS = *requireTLS

	_, _ = fmt.Fprintf(stdout, "LISTENING %s\n", ln.Addr().String())

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		_, _ = fmt.Fprintf(stderr, "shutting down...\n")
		_ = ln.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	return srv.Serve(ln)
}
