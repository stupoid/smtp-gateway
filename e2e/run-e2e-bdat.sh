#!/usr/bin/env bash
# BDAT / CHUNKING (RFC 3030) e2e test: send a message using BDAT chunks
# and verify the complete message is delivered.
set -euo pipefail
cd "$(dirname "$0")/.."
source "$(dirname "$0")/_e2e_tools.sh"

TMPDIR=$(mktemp -d)
POSTCAT_DIR="$TMPDIR/mail"
SERVER_ADDR="127.0.0.1:12531"
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

# --------------------------------------------------
echo ""
echo "=== Test 1: BDAT single chunk (LAST) ==="
# BDAT <size> LAST — the body follows immediately without dot-stuffing.
# The size includes the trailing CRLF after the body.
BODY="BDAT chunk 1 — single chunk message"
BODY_LEN=$(printf '%s\r\n' "$BODY" | wc -c)
printf "EHLO bdat.test\r\nMAIL FROM:<bdat@test>\r\nRCPT TO:<bdat@example.com>\r\nBDAT %d LAST\r\n%s\r\nQUIT\r\n" \
    "$BODY_LEN" "$BODY" \
    | "$NC" -w 3 127.0.0.1 12531 > "$TMPDIR/nc-bdat1.out" 2>&1
grep -q '250 2.0.0 OK' "$TMPDIR/nc-bdat1.out" \
    && pass "BDAT single chunk delivered" \
    || fail "BDAT single chunk — expected 250 OK"

echo ""
echo "=== Test 2: BDAT multiple chunks ==="
CHUNK1="First chunk content"
CHUNK2="Second chunk content"
CHUNK3="Third chunk — final"
CHUNK1_LEN=$(printf '%s\r\n' "$CHUNK1" | wc -c)
CHUNK2_LEN=$(printf '%s\r\n' "$CHUNK2" | wc -c)
CHUNK3_LEN=$(printf '%s\r\n' "$CHUNK3" | wc -c)
printf "EHLO bdat2.test\r\nMAIL FROM:<bdat2@test>\r\nRCPT TO:<bdat2@example.com>\r\nBDAT %d\r\n%s\r\nBDAT %d\r\n%s\r\nBDAT %d LAST\r\n%s\r\nQUIT\r\n" \
    "$CHUNK1_LEN" "$CHUNK1" \
    "$CHUNK2_LEN" "$CHUNK2" \
    "$CHUNK3_LEN" "$CHUNK3" \
    | "$NC" -w 3 127.0.0.1 12531 > "$TMPDIR/nc-bdat2.out" 2>&1
grep -q '250 2.0.0 OK' "$TMPDIR/nc-bdat2.out" \
    && pass "BDAT multi-chunk delivered" \
    || fail "BDAT multi-chunk — expected 250 OK"

sleep 0.5

# --------------------------------------------------
echo ""
echo "=== Stopping server ==="
kill "$SERVER_PID" 2>/dev/null || true
wait "$SERVER_PID" 2>/dev/null || true

echo ""
echo "=== Postcat verification ==="
"$TMPDIR/verify-postcat" "$POSTCAT_DIR"

COUNT=$(find "$POSTCAT_DIR" -name '*.eml' 2>/dev/null | wc -l)
[ "$COUNT" -eq 2 ] \
    && pass "2 postcat files (2 BDAT messages)" \
    || fail "expected 2 postcat files, got $COUNT"

# Single chunk message should contain the body verbatim.
grep -qF 'BDAT chunk 1' "$POSTCAT_DIR"/*.eml \
    && pass "single chunk body preserved" \
    || fail "single chunk body missing"

# Multi-chunk message should contain all chunks concatenated.
grep -qF 'First chunk content' "$POSTCAT_DIR"/*.eml \
    && pass "multi-chunk: first chunk present" \
    || fail "multi-chunk: first chunk missing"
grep -qF 'Third chunk' "$POSTCAT_DIR"/*.eml \
    && pass "multi-chunk: third chunk present" \
    || fail "multi-chunk: third chunk missing"

grep -qi "panic\|fatal" "$TMPDIR/server.err" 2>/dev/null \
    && fail "server panicked" \
    || pass "server did not panic"

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
