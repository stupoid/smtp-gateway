#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source "$(dirname "$0")/_e2e_tools.sh"

TMPDIR=$(mktemp -d)
POSTCAT_DIR="$TMPDIR/mail"
SERVER_ADDR="127.0.0.1:12525"
PASS=0
FAIL=0

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
echo "=== Test 1: basic single-recipient ==="
"$SWAKS" \
    --to alice@example.com \
    --from bob@example.net \
    --server "$SERVER_ADDR" \
    --body "Hello from e2e test" \
    --header-Subject "e2e test"

echo ""
echo "=== Test 2: multiple recipients + null sender ==="
"$SWAKS" \
    --to alice@example.com,carol@example.org \
    --from "" \
    --server "$SERVER_ADDR" \
    --body "null sender test" \
    --header-Subject "null sender"

sleep 0.5

echo ""
echo "=== Stopping server ==="
kill "$SERVER_PID" 2>/dev/null || true
wait "$SERVER_PID" 2>/dev/null || true

echo ""
echo "=== Postcat files ==="
ls -la "$POSTCAT_DIR/"

echo ""
echo "=== Raw postcat content ==="
for f in "$POSTCAT_DIR"/*.eml; do
    echo "--- $(basename "$f") ---"
    cat "$f"
    echo "---"
done

echo ""
echo "=== Verify with ParsePostcat ==="
./verify-postcat "$POSTCAT_DIR"

echo ""
echo "All checks passed."
