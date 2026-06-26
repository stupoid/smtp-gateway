#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source "$(dirname "$0")/_e2e_tools.sh"

TMPDIR=$(mktemp -d)
POSTCAT_DIR="$TMPDIR/mail"
SERVER_ADDR="127.0.0.1:12526"

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
echo "=== Test 1: null sender ==="
"$SWAKS" \
    --from '<>' \
    --to nulltest@example.com \
    --server "$SERVER_ADDR" \
    --body "null sender body" \
    --header-Subject "null sender"

echo ""
echo "=== Test 2: precise body ==="
"$SWAKS" \
    --from bob@x.com \
    --to alice@x.com \
    --server "$SERVER_ADDR" \
    --body "one
two
three" \
    --header-Subject "precise body"

sleep 0.5

echo ""
echo "=== Stopping server ==="
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
./verify-postcat "$POSTCAT_DIR"
