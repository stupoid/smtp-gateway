#!/usr/bin/env bash
# Test PIPELINING: send MAIL FROM + RCPT TO + DATA in one write.
set -euo pipefail
cd "$(dirname "$0")"
source "$(dirname "$0")/_e2e_tools.sh"

TMPDIR=$(mktemp -d)
POSTCAT_DIR="$TMPDIR/mail"
SERVER_ADDR="127.0.0.1:12527"
cleanup() { rm -rf "$TMPDIR"; }
trap cleanup EXIT

echo "=== Building ==="
go build -o test-server ./cmd/test-server/
go build -o verify-postcat ./cmd/verify-postcat/

echo "=== Starting server ==="
./test-server "$SERVER_ADDR" "$POSTCAT_DIR" > "$TMPDIR/server.out" 2> "$TMPDIR/server.err" &
SERVER_PID=$!
for i in $(seq 1 30); do
    if grep -q LISTENING "$TMPDIR/server.out" 2>/dev/null; then break; fi
    sleep 0.1
done
echo "Server ready."

echo ""
echo "=== Test: PIPELINING (send all commands without waiting) ==="
printf 'EHLO pipetest\r\nMAIL FROM:<pipe@test>\r\nRCPT TO:<a@x>\r\nDATA\r\nSubject: pipe\r\n\r\npipeline body\r\n.\r\nQUIT\r\n' | "$NC" -w 3 127.0.0.1 12527

sleep 0.5
kill "$SERVER_PID" 2>/dev/null || true
wait "$SERVER_PID" 2>/dev/null || true

echo ""
echo "=== Postcat files ==="
for f in "$POSTCAT_DIR"/*.eml; do
    echo "--- $(basename "$f") ---"
    cat "$f"
    echo "---"
done

echo ""
echo "=== Verify ==="
./verify-postcat "$POSTCAT_DIR" 2>&1

# Check the server didn't crash.
if grep -q "panic\|fatal" "$TMPDIR/server.err" 2>/dev/null; then
    echo "FAIL: server stderr has errors"
    cat "$TMPDIR/server.err"
    exit 1
fi
echo "Server clean, no panics."
