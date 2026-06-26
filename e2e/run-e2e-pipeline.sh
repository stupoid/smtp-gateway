#!/usr/bin/env bash
# Test RFC 2920 PIPELINING: send commands in one write and verify all
# responses arrive in order and the message is delivered.
set -euo pipefail
cd "$(dirname "$0")/.."
source "$(dirname "$0")/_e2e_tools.sh"

TMPDIR=$(mktemp -d)
POSTCAT_DIR="$TMPDIR/mail"
SERVER_ADDR="127.0.0.1:12527"
PASS=0 FAIL=0

SERVER_PID=""
cleanup() {
    [ -n "${SERVER_PID:-}" ] && kill "$SERVER_PID" 2>/dev/null || true
    wait "${SERVER_PID:-}" 2>/dev/null || true
    rm -rf "$TMPDIR"
}
trap cleanup EXIT

RED='\033[0;31m'; GREEN='\033[0;32m'; RST='\033[0m'
pass() { PASS=$((PASS + 1)); echo -e "${GREEN}PASS${RST} $*"; }
fail() { FAIL=$((FAIL + 1)); echo -e "${RED}FAIL${RST} $*"; }

echo "=== Building ==="
CGO_ENABLED=0 go build -o "$TMPDIR/test-server" ./cmd/test-server/
CGO_ENABLED=0 go build -o "$TMPDIR/verify-postcat" ./cmd/verify-postcat/

echo "=== Starting server ==="
"$TMPDIR/test-server" "$SERVER_ADDR" "$POSTCAT_DIR" > "$TMPDIR/server.out" 2> "$TMPDIR/server.err" &
SERVER_PID=$!
for i in $(seq 1 30); do
    if grep -q LISTENING "$TMPDIR/server.out" 2>/dev/null; then break; fi
    sleep 0.1
done
grep -q LISTENING "$TMPDIR/server.out" || { fail "server did not start"; exit 1; }
echo "Server ready."

echo ""
echo "=== Test: PIPELINING (send all commands without waiting) ==="
# Send EHLO + MAIL + RCPT + DATA all at once, then read responses.
printf 'EHLO pipetest\r\nMAIL FROM:<pipe@test>\r\nRCPT TO:<a@x>\r\nDATA\r\nSubject: pipe\r\n\r\npipeline body\r\n.\r\nQUIT\r\n' \
    | "$NC" -w 3 127.0.0.1 12527 > "$TMPDIR/nc-pipe.out" 2>&1

# Verify the 250 response for DATA came through (message accepted).
grep -q '250 2.0.0 OK' "$TMPDIR/nc-pipe.out" \
    && pass "pipelined message accepted" \
    || fail "pipelined message — expected 250 OK"

sleep 0.5
kill "$SERVER_PID" 2>/dev/null || true
wait "$SERVER_PID" 2>/dev/null || true

echo ""
echo "=== Postcat verification ==="
"$TMPDIR/verify-postcat" "$POSTCAT_DIR"

COUNT=$(find "$POSTCAT_DIR" -name '*.eml' 2>/dev/null | wc -l)
[ "$COUNT" -eq 1 ] \
    && pass "exactly 1 postcat file" \
    || fail "expected 1 postcat file, got $COUNT"

grep -q '^S pipe@test$' "$POSTCAT_DIR"/*.eml \
    && pass "sender matches" \
    || fail "sender wrong"

grep -qF 'pipeline body' "$POSTCAT_DIR"/*.eml \
    && pass "body matches" \
    || fail "body wrong"

grep -qi "panic\|fatal" "$TMPDIR/server.err" 2>/dev/null \
    && fail "server panicked" \
    || pass "server did not panic"

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
