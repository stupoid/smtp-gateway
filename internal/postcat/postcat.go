// Package postcat provides reading and writing of postcat-format envelope
// files, compatible with Postfix's postcat(1) output format.
//
// Files contain envelope records (S, R, T lines) followed by a blank line
// and the raw RFC 5322 message body.
package postcat

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Message is a parsed postcat-format email file.
type Message struct {
	Sender     string
	Recipients []string
	Time       time.Time
	RawMessage []byte // full RFC 5322 message (headers + body)
}

// Write writes an accepted message to dir.  The file is named
// <unix-timestamp>-<nanosecond>.eml and contains envelope records
// followed by a blank line and the raw message.
//
// The write is atomic: data is written to a temporary file and renamed
// into place on success.  This prevents both partial-file artefacts and
// filename collisions under concurrent writes.
func Write(dir, mailFrom string, accepted []string, body []byte) (string, error) {
	now := time.Now()
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	id := hex.EncodeToString(rnd[:])
	name := fmt.Sprintf("%d-%d-%s.eml", now.Unix(), now.Nanosecond(), id)
	path := filepath.Join(dir, name)

	f, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return path, fmt.Errorf("create postcat temp file: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()

	w := bufio.NewWriter(f)

	// Envelope records.
	if _, err := fmt.Fprintf(w, "S %s\n", sanitizeAddr(FormatNullSender(mailFrom))); err != nil {
		return path, fmt.Errorf("write sender record: %w", err)
	}
	for _, rcpt := range accepted {
		if _, err := fmt.Fprintf(w, "R %s\n", sanitizeAddr(rcpt)); err != nil {
			return path, fmt.Errorf("write recipient record: %w", err)
		}
	}
	if _, err := fmt.Fprintf(w, "T %s\n", now.Format(time.RFC3339)); err != nil {
		return path, fmt.Errorf("write timestamp record: %w", err)
	}

	// Blank envelope separator.
	if err := w.WriteByte('\n'); err != nil {
		return path, fmt.Errorf("write separator: %w", err)
	}

	// Raw message.
	if _, err := w.Write(body); err != nil {
		return path, fmt.Errorf("write body: %w", err)
	}

	if err := w.Flush(); err != nil {
		_ = f.Close()
		return path, fmt.Errorf("flush postcat file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return path, fmt.Errorf("sync postcat file: %w", err)
	}
	if err := f.Close(); err != nil {
		return path, fmt.Errorf("close postcat file: %w", err)
	}

	if err := os.Rename(f.Name(), path); err != nil {
		return path, fmt.Errorf("rename postcat file: %w", err)
	}
	return path, nil
}

// Parse reads a postcat-format file from path and returns the parsed
// message.  It reads envelope records (S, R, T lines) until a blank
// line, then treats the remainder as the raw RFC 5322 message.
//
// The raw body is read verbatim (no reconstruction) so the original
// content is preserved exactly — no spurious trailing CRLF is added.
func Parse(path string) (*Message, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open postcat file: %w", err)
	}

	m := &Message{}

	// Find the blank line separator between envelope and body.
	sepIdx, sepLen := findBlankLine(raw)
	if sepIdx < 0 {
		// No blank line separator; the file has no body.
		return m, nil
	}

	// Parse envelope records line by line.  Trim \r so both LF and
	// CRLF line endings are handled correctly.
	envelope := string(raw[:sepIdx])
	for _, line := range strings.Split(envelope, "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case strings.HasPrefix(line, "S "):
			m.Sender = line[2:]
		case strings.HasPrefix(line, "R "):
			m.Recipients = append(m.Recipients, line[2:])
		case strings.HasPrefix(line, "T "):
			t, err := time.Parse(time.RFC3339, line[2:])
			if err != nil {
				return nil, fmt.Errorf("parse timestamp record: %w", err)
			}
			m.Time = t
		}
	}

	// Body is everything after the blank line separator.
	m.RawMessage = raw[sepIdx+sepLen:]
	return m, nil
}

// findBlankLine returns the byte index of the blank line separator and its
// length (2 for "\n\n", 4 for "\r\n\r\n"), or (-1, 0) if not found.
func findBlankLine(b []byte) (int, int) {
	for i := 0; i < len(b)-1; i++ {
		if b[i] == '\n' && b[i+1] == '\n' {
			return i, 2
		}
	}
	// Also accept "\r\n\r\n" (CRLF blank line).
	for i := 0; i < len(b)-3; i++ {
		if b[i] == '\r' && b[i+1] == '\n' && b[i+2] == '\r' && b[i+3] == '\n' {
			return i, 4
		}
	}
	return -1, 0
}

// FormatNullSender returns "<>" for an empty or null sender, or the sender
// unchanged.  It normalises both "" and the literal "<>" to "<>".
func FormatNullSender(s string) string {
	if s == "" || s == "<>" {
		return "<>"
	}
	return s
}

// sanitizeAddr strips newline and carriage-return characters from an
// envelope address.  Without this, a crafted address containing "\nS
// attacker@evil.com" could inject fake envelope records into the
// postcat file, corrupting audit trails or misleading downstream tools.
func sanitizeAddr(a string) string {
	// Most addresses contain no newlines, so avoid allocation when possible.
	if !strings.ContainsAny(a, "\r\n") {
		return a
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, a)
}
