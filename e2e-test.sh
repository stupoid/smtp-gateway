#!/usr/bin/env bash
# e2e-test.sh — End-to-end test for smtp-gateway using swaks.
# Requires: swaks, go (or the pre-built test-server binary).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$SCRIPT_DIR/.."
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

POSTCAT_DIR="$TMPDIR/mail"
SERVER_ADDR="127.0.0.1:12525"
SERVER_BIN="$PROJECT_DIR/test-server"

echo "=== Building test-server ==="
cd "$PROJECT_DIR"
CGO_ENABLED=0 go build -o "$SERVER_BIN" ./cmd/test-server/

echo "=== Starting server on $SERVER_ADDR ==="
"$SERVER_BIN" "$SERVER_ADDR" "$POSTCAT_DIR" > "$TMPDIR/server.out" 2> "$TMPDIR/server.err" &
SERVER_PID=$!

# Wait for the server to print LISTENING.
for i in $(seq 1 30); do
    if grep -q LISTENING "$TMPDIR/server.out" 2>/dev/null; then
        break
    fi
    sleep 0.1
done
if ! grep -q LISTENING "$TMPDIR/server.out" 2>/dev/null; then
    echo "ERROR: server did not start"
    cat "$TMPDIR/server.err"
    exit 1
fi
echo "Server ready."

echo "=== Sending test email via swaks ==="
swaks --to alice@example.com \
      --from bob@example.net \
      --server "$SERVER_ADDR" \
      --data - <<'EOF'
From: Bob <bob@example.net>
To: Alice <alice@example.com>
Subject: e2e test

Hello from the e2e test!
EOF

echo "=== Sending second email (multiple recipients) ==="
swaks --to alice@example.com,carol@example.org \
      --from "" \
      --server "$SERVER_ADDR" \
      --data - <<'EOF'
From: <>
To: undisclosed-recipients:;
Subject: null sender test

MAIL FROM:<> test.
EOF

# Give the server a moment to flush files.
sleep 0.5

echo "=== Stopping server ==="
kill "$SERVER_PID" 2>/dev/null || true
wait "$SERVER_PID" 2>/dev/null || true

echo ""
echo "=== Postcat files ==="
ls -la "$POSTCAT_DIR/"

# Verify with postcat-compatible output (just cat for now).
echo ""
echo "=== Verifying postcat files ==="
for f in "$POSTCAT_DIR"/*.eml; do
    echo "--- $f ---"
    cat "$f"
    echo "---"
done

# Parse and verify using a small Go program.
echo ""
echo "=== Parsing with ParsePostcat ==="
cat > "$TMPDIR/verify.go" <<'GOEOF'
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/stupoid/smtp-gateway"
)

func main() {
	dir := os.Args[1]
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "readdir: %v\n", err)
		os.Exit(1)
	}
	pass := 0
	fail := 0
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		msg, err := smtpgateway.ParsePostcat(path)
		if err != nil {
			fmt.Printf("FAIL %s: parse error: %v\n", e.Name(), err)
			fail++
			continue
		}
		fmt.Printf("OK   %s  sender=%q  recipients=%v  time=%s  body_len=%d\n",
			e.Name(), msg.Sender, msg.Recipients, msg.Time.Format("15:04:05"), len(msg.RawMessage))
		pass++
	}
	fmt.Printf("\n%d passed, %d failed\n", pass, fail)
	if fail > 0 {
		os.Exit(1)
	}
}
GOEOF

CGO_ENABLED=0 go run "$TMPDIR/verify.go" "$POSTCAT_DIR"
