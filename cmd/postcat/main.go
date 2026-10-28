// postcat reads a postcat-format envelope file and prints its contents.
//
// Usage:
//
//	postcat <file.eml>
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/stupoid/smtp-gateway/internal/postcat"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		usage(stderr)
		return 0
	}
	if len(args) < 1 {
		usage(stderr)
		return 1
	}

	msg, err := postcat.Parse(args[0])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "postcat: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "Sender:     %s\n", msg.Sender)
	_, _ = fmt.Fprintf(stdout, "Recipients: %s\n", fmt.Sprint(msg.Recipients))
	_, _ = fmt.Fprintf(stdout, "Time:       %s\n", msg.Time.Format("2006-01-02 15:04:05"))
	_, _ = fmt.Fprintf(stdout, "Body size:  %d bytes\n", len(msg.RawMessage))
	_, _ = fmt.Fprintf(stdout, "\n--- Raw message ---\n%s", string(msg.RawMessage))
	return 0
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintf(w, "Usage: postcat <file.eml>\n")
	_, _ = fmt.Fprintf(w, "Print the decoded envelope and body of a postcat-format file.\n")
}
