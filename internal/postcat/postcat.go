// Package postcat provides reading and writing of postcat-format envelope
// files, compatible with Postfix's postcat(1) output format.
//
// Files contain envelope records (S, R, T lines) followed by a blank line
// and the raw RFC 5322 message body.
package postcat

import (
	"bufio"
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
func Write(dir, mailFrom string, accepted []string, body []byte) (string, error) {
	now := time.Now()
	name := fmt.Sprintf("%d-%d.eml", now.Unix(), now.Nanosecond())
	path := filepath.Join(dir, name)

	f, err := os.Create(path)
	if err != nil {
		return path, fmt.Errorf("create postcat file: %w", err)
	}
	defer func() { _ = f.Close() }()

	w := bufio.NewWriter(f)

	// Envelope records.
	if _, err := fmt.Fprintf(w, "S %s\n", FormatNullSender(mailFrom)); err != nil {
		return path, fmt.Errorf("write sender record: %w", err)
	}
	for _, rcpt := range accepted {
		if _, err := fmt.Fprintf(w, "R %s\n", rcpt); err != nil {
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

	return path, w.Flush()
}

// Parse reads a postcat-format file from path and returns the parsed
// message.  It reads envelope records (S, R, T lines) until a blank
// line, then treats the remainder as the raw RFC 5322 message.
func Parse(path string) (*Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open postcat file: %w", err)
	}
	defer func() { _ = f.Close() }()

	m := &Message{}
	scanner := bufio.NewScanner(f)
	inBody := false
	var bodyLines []string

	for scanner.Scan() {
		line := scanner.Text()
		if !inBody {
			if line == "" {
				inBody = true
				continue
			}
			switch {
			case strings.HasPrefix(line, "S "):
				m.Sender = line[2:]
			case strings.HasPrefix(line, "R "):
				m.Recipients = append(m.Recipients, line[2:])
			case strings.HasPrefix(line, "T "):
				t, err := time.Parse(time.RFC3339, line[2:])
				if err == nil {
					m.Time = t
				}
			}
		} else {
			bodyLines = append(bodyLines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read postcat file: %w", err)
	}

	// Reconstruct raw message with CRLF line endings.
	raw := strings.Join(bodyLines, "\r\n")
	if len(bodyLines) > 0 {
		raw += "\r\n"
	}
	m.RawMessage = []byte(raw)

	return m, nil
}

// FormatNullSender returns "<>" for an empty or null sender, or the sender
// unchanged.  It normalises both "" and the literal "<>" to "<>".
func FormatNullSender(s string) string {
	if s == "" || s == "<>" {
		return "<>"
	}
	return s
}
