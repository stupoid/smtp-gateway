// Package postcat reads and writes postcat-format envelope files.
//
// The layout borrows the S, R, and T record letters that Postfix writes in
// queue files. It is not the Postfix queue-file format, and Postfix tools
// do not expect it.
//
// A file contains envelope records (S, R, T lines), a blank line, and the
// raw RFC 5322 message body.
package postcat

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
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

// FormatEnvelope returns the complete postcat-format bytes for a message:
// envelope records (S, R, T lines), a blank separator, and the raw body.
// It uses writeEnvelope, the same record writer as Write.
func FormatEnvelope(mailFrom string, accepted []string, t time.Time, body []byte) []byte {
	var buf bytes.Buffer
	_ = writeEnvelope(&buf, mailFrom, accepted, t)
	buf.Write(body)
	return buf.Bytes()
}

// writeEnvelope writes the S, R, and T records and the blank separator.
// It is the single record writer for FormatEnvelope and Write.
func writeEnvelope(w io.Writer, mailFrom string, accepted []string, t time.Time) error {
	if _, err := fmt.Fprintf(w, "S %s\n", sanitizeAddr(FormatNullSender(mailFrom))); err != nil {
		return fmt.Errorf("write sender record: %w", err)
	}
	for _, rcpt := range accepted {
		if _, err := fmt.Fprintf(w, "R %s\n", sanitizeAddr(rcpt)); err != nil {
			return fmt.Errorf("write recipient record: %w", err)
		}
	}
	if _, err := fmt.Fprintf(w, "T %s\n", t.Format(time.RFC3339)); err != nil {
		return fmt.Errorf("write timestamp record: %w", err)
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return fmt.Errorf("write separator: %w", err)
	}
	return nil
}

// Write writes an accepted message to dir.  The file is named
// <unix-timestamp>-<nanosecond>-<random>.eml (the random hex suffix
// prevents collisions under concurrent writes) and contains envelope
// records followed by a blank line and the raw message.
//
// The write is atomic in the directory: data is written to a temporary
// file and renamed into place on success.  The file is fsynced, and the
// directory is synced where the OS supports it.  A power loss can still
// lose the file on a filesystem without directory sync.
//
// Every error path closes the temporary file and removes it.
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
	tmp := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()

	if err := writeEnvelope(f, mailFrom, accepted, now); err != nil {
		return path, err
	}
	if _, err := f.Write(body); err != nil {
		return path, fmt.Errorf("write body: %w", err)
	}
	if err := f.Sync(); err != nil {
		return path, fmt.Errorf("sync postcat file: %w", err)
	}
	if err := f.Close(); err != nil {
		return path, fmt.Errorf("close postcat file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return path, fmt.Errorf("rename postcat file: %w", err)
	}
	cleanup = false

	syncDir(dir)
	return path, nil
}

// syncDir flushes the directory entry after a rename.  Some filesystems do
// not support directory fsync, so the error is ignored.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
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

// findBlankLine returns the byte index of the earliest blank line separator
// and its length (2 for "\n\n", 4 for "\r\n\r\n"), or (-1, 0) if not found.
// The earliest match is the envelope separator; a later match is body text.
func findBlankLine(b []byte) (int, int) {
	lf := bytes.Index(b, []byte("\n\n"))
	crlf := bytes.Index(b, []byte("\r\n\r\n"))
	switch {
	case lf < 0 && crlf < 0:
		return -1, 0
	case lf < 0:
		return crlf, 4
	case crlf < 0:
		return lf, 2
	case lf < crlf:
		return lf, 2
	default:
		return crlf, 4
	}
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
