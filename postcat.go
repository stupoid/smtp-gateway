package smtpgateway

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PostcatMessage is a parsed postcat-format email file.
type PostcatMessage struct {
	Sender     string
	Recipients []string
	Time       time.Time
	RawMessage []byte // full RFC 5322 message (headers + body)
}

// WritePostcat writes an accepted message to dir in a format compatible
// with Postfix's postcat(1) utility.  The file is named
// <unix-timestamp>-<sequence> and contains envelope records followed
// by a blank line and the raw message.
//
// Returns the full path to the written file.
func WritePostcat(dir string, tx *Tx, body []byte) (string, error) {
	now := time.Now()

	// Simple unique filename: timestamp + nanosecond.
	name := fmt.Sprintf("%d-%d.eml", now.Unix(), now.Nanosecond())
	path := filepath.Join(dir, name)

	f, err := os.Create(path)
	if err != nil {
		return path, fmt.Errorf("create postcat file: %w", err)
	}
	defer func() { _ = f.Close() }()

	w := bufio.NewWriter(f)

	// Envelope records.
	_, _ = fmt.Fprintf(w, "S %s\n", senderOrEmpty(tx.MailFrom))
	for _, rcpt := range tx.Accepted {
		_, _ = fmt.Fprintf(w, "R %s\n", rcpt)
	}
	_, _ = fmt.Fprintf(w, "T %s\n", now.Format(time.RFC3339))

	// Blank envelope separator.
	_ = w.WriteByte('\n')

	// Raw message.
	_, _ = w.Write(body)

	return path, w.Flush()
}

// ParsePostcat reads a postcat-format file from path and returns the
// parsed message.  It reads envelope records (S, R, T lines) until a
// blank line, then treats the remainder as the raw RFC 5322 message.
func ParsePostcat(path string) (*PostcatMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open postcat file: %w", err)
	}
	defer func() { _ = f.Close() }()

	m := &PostcatMessage{}
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

func senderOrEmpty(s string) string {
	if s == "" {
		return "<>"
	}
	return s
}
