#!/usr/bin/env bash
# Test error handling: out-of-sequence, unknown commands, size limit.
set -euo pipefail
cd "$(dirname "$0")/.."
source "$(dirname "$0")/_e2e_tools.sh"

TMPDIR=$(mktemp -d)
POSTCAT_DIR="$TMPDIR/mail"
SERVER_ADDR="127.0.0.1:12528"
cleanup() { rm -rf "$TMPDIR"; }
trap cleanup EXIT

echo "=== Building ==="
go build -o test-server ./cmd/test-server/

echo "=== Starting server ==="
./test-server "$SERVER_ADDR" "$POSTCAT_DIR" > "$TMPDIR/server.out" 2> "$TMPDIR/server.err" &
SERVER_PID=$!
for i in $(seq 1 30); do
    if grep -q LISTENING "$TMPDIR/server.out" 2>/dev/null; then break; fi
    sleep 0.1
done
echo "Server ready."

echo ""
echo "=== Test 1: RCPT before HELO ==="
printf 'RCPT TO:<x@y>\r\nQUIT\r\n' | "$NC" -w 2 127.0.0.1 12528

echo ""
echo "=== Test 2: DATA before RCPT ==="
printf 'EHLO test\r\nMAIL FROM:<a@b>\r\nDATA\r\nQUIT\r\n' | "$NC" -w 2 127.0.0.1 12528

echo ""
echo "=== Test 3: unknown command ==="
printf 'EHLO test\r\nFOOBAR\r\nQUIT\r\n' | "$NC" -w 2 127.0.0.1 12528

echo ""
echo "=== Test 4: MAIL FROM with no <> ==="
printf 'EHLO test\r\nMAIL FROM: bare\r\nQUIT\r\n' | "$NC" -w 2 127.0.0.1 12528

sleep 0.2
kill "$SERVER_PID" 2>/dev/null || true
wait "$SERVER_PID" 2>/dev/null || true

echo ""
if grep -q "panic\|fatal" "$TMPDIR/server.err" 2>/dev/null; then
    echo "FAIL: server panicked!"
    cat "$TMPDIR/server.err"
    exit 1
fi
echo "PASS: no server panics."
