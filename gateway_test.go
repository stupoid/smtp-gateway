package smtpgateway

import (
	"crypto/tls"
	"log/slog"
	"os"
	"testing"
	"time"
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
		WithReadTimeout(30*time.Second),
		WithWriteTimeout(10*time.Second),
		WithIdleTimeout(60*time.Second),
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
	if s.ReadTimeout != 30*time.Second {
		t.Errorf("ReadTimeout = %v", s.ReadTimeout)
	}
	if s.WriteTimeout != 10*time.Second {
		t.Errorf("WriteTimeout = %v", s.WriteTimeout)
	}
	if s.IdleTimeout != 60*time.Second {
		t.Errorf("IdleTimeout = %v", s.IdleTimeout)
	}
	if s.Logger != l {
		t.Error("Logger not set")
	}
	if s.PostcatDir != "/tmp/test" {
		t.Errorf("PostcatDir = %q", s.PostcatDir)
	}
}

func TestSlogLoggerNilGuard(t *testing.T) {
	// NewSlogLogger(nil) returns a no-op Logger.
	l := NewSlogLogger(nil)
	if l == nil {
		t.Fatal("NewSlogLogger(nil) returned nil")
	}
	// Methods must not panic.
	l.Debug("test")
	l.Info("test")
	l.Error("test")

	// Nil SlogLogger pointer through interface must also be safe.
	var nl *SlogLogger
	nl.Debug("test")
	nl.Info("test")
	nl.Error("test")
}

func TestNewServerRequireTLS(t *testing.T) {
	s := NewServer(&acceptAllHandler{})
	if s.RequireTLS {
		t.Error("RequireTLS default = true, want false")
	}

	s = NewServer(&acceptAllHandler{}, WithRequireTLS(true))
	if !s.RequireTLS {
		t.Error("WithRequireTLS(true) not applied")
	}
}
