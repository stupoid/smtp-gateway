// verify-postcat reads postcat files and prints a summary.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

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

	entries, err := os.ReadDir(args[0])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "readdir: %v\n", err)
		return 1
	}

	pass := 0
	for _, e := range entries {
		path := filepath.Join(args[0], e.Name())
		msg, err := postcat.Parse(path)
		if err != nil {
			_, _ = fmt.Fprintf(stdout, "FAIL %s: %v\n", e.Name(), err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "OK   %s  sender=%-20s  recipients=%-40s  body_len=%d\n",
			e.Name(), msg.Sender, fmt.Sprint(msg.Recipients), len(msg.RawMessage))
		pass++
	}
	_, _ = fmt.Fprintf(stdout, "\n%d postcat file(s) verified OK\n", pass)
	return 0
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintf(w, "Usage: verify-postcat <directory>\n")
	_, _ = fmt.Fprintf(w, "Scan a directory of postcat files and report parse errors.\n")
}
