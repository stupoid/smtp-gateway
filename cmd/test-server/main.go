// test-server is a minimal SMTP server for end-to-end testing.
// It accepts all mail and writes it to postcat files.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
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
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run() error {
	tlsCert := flag.String("tls-cert", "", "TLS certificate file (enables STARTTLS)")
	tlsKey := flag.String("tls-key", "", "TLS key file (enables STARTTLS)")
	flag.Parse()

	if flag.NArg() < 2 {
		return errors.New("usage: test-server [-tls-cert cert.pem -tls-key key.pem] <listen-addr> <postcat-dir>")
	}
	addr := flag.Arg(0)
	postcatDir := flag.Arg(1)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer func() { _ = ln.Close() }()

	srv := &smtpgateway.Server{
		Hostname:     "test.local",
		Handler:      &acceptAllHandler{},
		PostcatDir:   postcatDir,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	if *tlsCert != "" && *tlsKey != "" {
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			return fmt.Errorf("load TLS key pair: %w", err)
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	}

	fmt.Printf("LISTENING %s\n", ln.Addr().String())

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		fmt.Fprintf(os.Stderr, "shutting down...\n")
		_ = ln.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	return srv.Serve(ln)
}
