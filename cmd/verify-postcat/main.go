// verify-postcat reads postcat files and prints a summary.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	smtp "github.com/stupoid/smtp-gateway"
)

func main() {
	entries, err := os.ReadDir(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "readdir: %v\n", err)
		os.Exit(1)
	}
	pass := 0
	for _, e := range entries {
		path := filepath.Join(os.Args[1], e.Name())
		msg, err := smtp.ParsePostcat(path)
		if err != nil {
			fmt.Printf("FAIL %s: %v\n", e.Name(), err)
			os.Exit(1)
		}
		fmt.Printf("OK   %s  sender=%-20s  recipients=%-40s  body_len=%d\n",
			e.Name(), msg.Sender, fmt.Sprint(msg.Recipients), len(msg.RawMessage))
		pass++
	}
	fmt.Printf("\n%d postcat file(s) verified OK\n", pass)
}
