// postcat reads a postcat-format envelope file and prints its contents.
//
// Usage:
//
//	postcat <file.eml>
package main

import (
	"fmt"
	"os"

	"github.com/stupoid/smtp-gateway/internal/postcat"
)

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "-h" || os.Args[1] == "--help") {
		fmt.Fprintf(os.Stderr, "Usage: postcat <file.eml>\n")
		fmt.Fprintf(os.Stderr, "Print the decoded envelope and body of a postcat-format file.\n")
		os.Exit(0)
	}
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: postcat <file.eml>\n")
		os.Exit(1)
	}

	msg, err := postcat.Parse(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "postcat: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Sender:     %s\n", msg.Sender)
	fmt.Printf("Recipients: %s\n", fmt.Sprint(msg.Recipients))
	fmt.Printf("Time:       %s\n", msg.Time.Format("2006-01-02 15:04:05"))
	fmt.Printf("Body size:  %d bytes\n", len(msg.RawMessage))
	fmt.Printf("\n--- Raw message ---\n%s", string(msg.RawMessage))
}
