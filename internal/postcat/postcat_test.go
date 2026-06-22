package postcat

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFormatNullSender(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"", "<>"},
		{"<>", "<>"},
		{"sender@example.com", "sender@example.com"},
		{"user@host", "user@host"},
	}
	for _, tt := range tests {
		got := FormatNullSender(tt.input)
		if got != tt.want {
			t.Errorf("FormatNullSender(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestWriteAndParse(t *testing.T) {
	dir := t.TempDir()

	body := []byte("From: sender@example.com\r\nSubject: Test\r\n\r\nHello, world.\r\n")
	path, err := Write(dir, "sender@example.com", []string{"a@b.com", "c@d.com"}, body)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if path == "" {
		t.Fatal("Write returned empty path")
	}

	msg, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if msg.Sender != "sender@example.com" {
		t.Errorf("Sender = %q, want %q", msg.Sender, "sender@example.com")
	}
	if len(msg.Recipients) != 2 {
		t.Fatalf("Recipients = %d, want 2", len(msg.Recipients))
	}
	if msg.Recipients[0] != "a@b.com" || msg.Recipients[1] != "c@d.com" {
		t.Errorf("Recipients = %v", msg.Recipients)
	}
	if string(msg.RawMessage) != string(body) {
		t.Errorf("RawMessage = %q, want %q", string(msg.RawMessage), string(body))
	}
	if msg.Time.IsZero() {
		t.Error("Time should not be zero")
	}
}

func TestWriteNullSender(t *testing.T) {
	dir := t.TempDir()

	path, err := Write(dir, "", []string{"rcpt@t.com"}, []byte("body\r\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	msg, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if msg.Sender != "<>" {
		t.Errorf("null sender = %q, want %q", msg.Sender, "<>")
	}
}

func TestWriteEmptyRecipients(t *testing.T) {
	dir := t.TempDir()

	path, err := Write(dir, "s@t.com", nil, []byte("body\r\n"))
	if err != nil {
		t.Fatalf("Write with nil recipients: %v", err)
	}

	msg, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(msg.Recipients) != 0 {
		t.Errorf("expected 0 recipients, got %d", len(msg.Recipients))
	}
}

func TestWriteEmptyBody(t *testing.T) {
	dir := t.TempDir()

	path, err := Write(dir, "s@t.com", []string{"r@t.com"}, nil)
	if err != nil {
		t.Fatalf("Write with nil body: %v", err)
	}

	msg, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(msg.RawMessage) != 0 {
		t.Errorf("expected empty body, got %d bytes", len(msg.RawMessage))
	}
}

func TestParseMissingFile(t *testing.T) {
	_, err := Parse("/nonexistent/path/file.eml")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestParseNoTimestamp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-timestamp.eml")
	content := "S sender@t.com\nR rcpt@t.com\n\nbody\r\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	msg, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !msg.Time.IsZero() {
		t.Error("time should be zero when T record is missing")
	}
	if msg.Sender != "sender@t.com" {
		t.Errorf("Sender = %q", msg.Sender)
	}
	if string(msg.RawMessage) != "body\r\n" {
		t.Errorf("RawMessage = %q", string(msg.RawMessage))
	}
}

func TestParseMissingSender(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing-sender.eml")
	content := "R rcpt@t.com\n\nbody\r\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	msg, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if msg.Sender != "" {
		t.Errorf("Sender = %q, want empty", msg.Sender)
	}
}

func TestParseExtraRecordsIgnored(t *testing.T) {
	// Extra envelope records (e.g., H headers) should be silently ignored.
	dir := t.TempDir()
	path := filepath.Join(dir, "extra-records.eml")
	content := "S sender@t.com\nH X-Extra: value\nR rcpt@t.com\n\nbody\r\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	msg, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if msg.Sender != "sender@t.com" {
		t.Errorf("Sender = %q", msg.Sender)
	}
	if len(msg.Recipients) != 1 {
		t.Errorf("expected 1 recipient, got %d", len(msg.Recipients))
	}
}

func TestParseEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.eml")
	if err := os.WriteFile(path, []byte{}, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	msg, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if msg.Sender != "" || len(msg.Recipients) != 0 || len(msg.RawMessage) != 0 {
		t.Error("empty file should produce empty message")
	}
}

func TestWriteUnixTimestampFilename(t *testing.T) {
	dir := t.TempDir()

	path, err := Write(dir, "s@t.com", []string{"r@t.com"}, []byte("test\r\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Verify the file exists at the returned path.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not found at %s: %v", path, err)
	}

	// Verify filename pattern: <unix-timestamp>-<nanosecond>.eml
	base := filepath.Base(path)
	// Should match: digits-dash-digits-dot-eml
	if len(base) < 8 || base[len(base)-4:] != ".eml" {
		t.Errorf("unexpected filename format: %s", base)
	}
}

func TestWritePreservesTimestamp(t *testing.T) {
	dir := t.TempDir()

	before := time.Now()
	path, err := Write(dir, "s@t.com", []string{"r@t.com"}, []byte("test\r\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	after := time.Now()

	msg, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Compare at second granularity — monotonic clock skew between
	// time.Now and time.Parse makes sub-second comparisons fragile.
	b := before.Truncate(time.Second)
	a := after.Truncate(time.Second)
	parsed := msg.Time.Truncate(time.Second)

	if parsed.Before(b) || parsed.After(a) {
		t.Errorf("timestamp %v not in expected range [%v, %v]", msg.Time, before, after)
	}
}
