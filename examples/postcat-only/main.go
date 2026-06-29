// postcat-only is a minimal SMTP gateway server that persists every accepted
// message as a Postfix-compatible postcat file on disk.  No cloud credentials,
// no compression — just drop mail into a directory.
//
// Configuration is via environment variables:
//
//	SMTP_LISTEN    — listen address (default: :2525)
//	SMTP_HOSTNAME  — server hostname (default: system hostname)
//	POSTCAT_DIR    — output directory (default: /tmp/postcat-mail)
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	smtpgateway "github.com/stupoid/smtp-gateway"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	hostname := os.Getenv("SMTP_HOSTNAME")
	listen := os.Getenv("SMTP_LISTEN")
	if listen == "" {
		listen = ":2525"
	}
	postcatDir := os.Getenv("POSTCAT_DIR")
	if postcatDir == "" {
		postcatDir = "/tmp/postcat-mail"
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer ln.Close()

	srv := smtpgateway.NewServer(
		&handler{logger: logger},
		smtpgateway.WithHostname(hostname),
		smtpgateway.WithPostcatDir(postcatDir),
		smtpgateway.WithMaxConnections(100),
		smtpgateway.WithReadTimeout(5*time.Minute),
		smtpgateway.WithWriteTimeout(1*time.Minute),
		smtpgateway.WithIdleTimeout(5*time.Minute),
		smtpgateway.WithLogger(smtpgateway.NewSlogLogger(logger)),
	)

	logger.Info("listening", "addr", ln.Addr(), "postcat_dir", postcatDir)

	// Graceful shutdown.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		logger.Info("shutting down", "signal", sig.String())
		ln.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("shutdown timeout", "error", err)
		}
	}()

	return srv.Serve(ln)
}

// handler accepts everything and logs each phase.
type handler struct {
	logger *slog.Logger
}

func (h *handler) Hello(_ context.Context, tx *smtpgateway.Tx) *smtpgateway.Response {
	h.logger.Info("hello", "domain", tx.Helo, "remote", tx.RemoteAddr)
	return smtpgateway.RespHelloOK
}

func (h *handler) MailFrom(_ context.Context, tx *smtpgateway.Tx) *smtpgateway.Response {
	sender := tx.MailFrom
	if sender == "" {
		sender = "<>"
	}
	h.logger.Info("mail_from", "sender", sender)
	return smtpgateway.RespMailOK
}

func (h *handler) RcptTo(_ context.Context, tx *smtpgateway.Tx) *smtpgateway.Response {
	last := tx.Rcpts[len(tx.Rcpts)-1]
	h.logger.Info("rcpt_to", "recipient", last)
	return smtpgateway.RespRcptOK
}

func (h *handler) Data(_ context.Context, tx *smtpgateway.Tx, body []byte) *smtpgateway.Response {
	h.logger.Info("data",
		"from", tx.MailFrom,
		"recipients", len(tx.Accepted),
		"bytes", len(body),
	)
	return smtpgateway.RespDataOK
}
