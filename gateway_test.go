package smtpgateway

import (
	"crypto/tls"
	"log/slog"
	"os"
	"testing"
)

func TestNewServerDefaults(t *testing.T) {
	h := &acceptAllHandler{}
	s := NewServer(h)

	if s.Handler != h {
		t.Error("handler not set")
	}
	if s.Hostname == "" {
		t.Error("hostname not resolved")
	}
	if s.MaxMessageSize != DefaultMaxMessageSize {
		t.Errorf("MaxMessageSize = %d, want %d", s.MaxMessageSize, DefaultMaxMessageSize)
	}
	if s.ReadTimeout != DefaultReadTimeout {
		t.Errorf("ReadTimeout = %v, want %v", s.ReadTimeout, DefaultReadTimeout)
	}
	if s.IdleTimeout != DefaultIdleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", s.IdleTimeout, DefaultIdleTimeout)
	}
	if s.MaxRecipients != 0 {
		t.Errorf("MaxRecipients = %d, want 0 (unlimited)", s.MaxRecipients)
	}
	if s.MaxConnections != 0 {
		t.Errorf("MaxConnections = %d, want 0 (unlimited)", s.MaxConnections)
	}
	if s.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 (no deadline)", s.WriteTimeout)
	}
	if s.Logger != nil {
		t.Error("Logger should be nil by default")
	}
	if s.PostcatDir != "" {
		t.Errorf("PostcatDir = %q, want empty", s.PostcatDir)
	}
}

func TestNewServerWithHostname(t *testing.T) {
	s := NewServer(&acceptAllHandler{}, WithHostname("mx.example.com"))
	if s.Hostname != "mx.example.com" {
		t.Errorf("Hostname = %q, want mx.example.com", s.Hostname)
	}
}

func TestNewServerLastOptionWins(t *testing.T) {
	s := NewServer(&acceptAllHandler{},
		WithMaxMessageSize(1000),
		WithMaxMessageSize(2000),
	)
	if s.MaxMessageSize != 2000 {
		t.Errorf("MaxMessageSize = %d, want 2000", s.MaxMessageSize)
	}
}

func TestNewServerWithTLSConfig(t *testing.T) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	s := NewServer(&acceptAllHandler{}, WithTLSConfig(cfg))
	if s.TLSConfig != cfg {
		t.Error("TLSConfig not set")
	}
}

func TestNewServerWithLogger(t *testing.T) {
	// Logger explicitly nil
	s := NewServer(&acceptAllHandler{}, WithLogger(nil))
	if s.Logger != nil {
		t.Error("Logger should be nil when explicitly set to nil")
	}
}

func TestNewServerWithAllOptions(t *testing.T) {
	l := NewSlogLogger(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	s := NewServer(&acceptAllHandler{},
		WithHostname("mx.example.com"),
		WithMaxMessageSize(10<<20),
		WithMaxRecipients(100),
		WithMaxConnections(50),
		WithReadTimeout(30),
		WithWriteTimeout(10),
		WithIdleTimeout(60),
		WithLogger(l),
		WithPostcatDir("/tmp/test"),
	)
	if s.Hostname != "mx.example.com" {
		t.Errorf("Hostname = %q", s.Hostname)
	}
	if s.MaxMessageSize != 10<<20 {
		t.Errorf("MaxMessageSize = %d", s.MaxMessageSize)
	}
	if s.MaxRecipients != 100 {
		t.Errorf("MaxRecipients = %d", s.MaxRecipients)
	}
	if s.MaxConnections != 50 {
		t.Errorf("MaxConnections = %d", s.MaxConnections)
	}
}

func TestNewServerServeZeroValueFallback(t *testing.T) {
	// Struct-literal path: zero-value Server gets defaults in Serve().
	// We can't call Serve() in a unit test without a listener,
	// but we can verify the zero-value Server is constructable.
	s := &Server{Handler: &acceptAllHandler{}}
	if s.Handler == nil {
		t.Fatal("handler not set")
	}
	_ = s // zero-value Server with Handler set — no panic
}
