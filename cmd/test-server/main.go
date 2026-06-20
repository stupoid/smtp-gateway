// test-server is a minimal SMTP server for end-to-end testing.
// It accepts all mail and writes it to postcat files.
package main

import (
	"context"
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
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: test-server <listen-addr> <postcat-dir>\n")
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

	srv := &smtpgateway.Server{
		Hostname:     "test.local",
		Handler:      &acceptAllHandler{},
		PostcatDir:   postcatDir,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
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

	if err := srv.Serve(ln); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}
