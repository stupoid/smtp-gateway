#!/usr/bin/env bash
# Basic delivery e2e tests: single rcpt, multi rcpt, null sender, dot-stuffed
# body.
set -euo pipefail
cd "$(dirname "$0")/.."
source "$(dirname "$0")/_e2e_tools.sh"

TMPDIR=$(mktemp -d)
POSTCAT_DIR="$TMPDIR/mail"
SERVER_ADDR="127.0.0.1:12525"
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
grep -q LISTENING "$TMPDIR/server.out" || { fail "server did not start"; cat "$TMPDIR/server.err"; exit 1; }
echo "Server ready."

# --------------------------------------------------
echo ""
echo "=== Test 1: single recipient ==="
"$SWAKS" \
    --to alice@example.com \
    --from bob@example.net \
    --server "$SERVER_ADDR" \
    --body "Hello from e2e test" \
    --header-Subject "e2e test" \
    > "$TMPDIR/swaks-1.out" 2>&1
grep -q '<-  250 2.0.0 OK' "$TMPDIR/swaks-1.out" \
    && pass "single recipient delivered" \
    || fail "single recipient — expected 250 OK"

# --------------------------------------------------
echo ""
echo "=== Test 2: multiple recipients ==="
"$SWAKS" \
    --to alice@example.com,carol@example.org \
    --from sender@example.net \
    --server "$SERVER_ADDR" \
    --body "multi recipient test" \
    --header-Subject "multi rcpt" \
    > "$TMPDIR/swaks-2.out" 2>&1
grep -q '<-  250 2.0.0 OK' "$TMPDIR/swaks-2.out" \
    && pass "multiple recipients delivered" \
    || fail "multiple recipients — expected 250 OK"

# --------------------------------------------------
echo ""
echo "=== Test 3: null sender (MAIL FROM:<>) ==="
"$SWAKS" \
    --from '<>' \
    --to nulltest@example.com \
    --server "$SERVER_ADDR" \
    --body "null sender body" \
    --header-Subject "null sender" \
    > "$TMPDIR/swaks-3.out" 2>&1
grep -q '<-  250 2.0.0 OK' "$TMPDIR/swaks-3.out" \
    && pass "null sender delivered" \
    || fail "null sender — expected 250 OK"

# --------------------------------------------------
echo ""
echo "=== Test 4: body with leading dots (dot-stuffing) ==="
"$SWAKS" \
    --from dot@test.local \
    --to dotrcpt@example.com \
    --server "$SERVER_ADDR" \
    --body ".leading dot
..double dot
...triple dot
normal line
.another leading" \
    --header-Subject "dot-stuffing test" \
    > "$TMPDIR/swaks-4.out" 2>&1
grep -q '<-  250 2.0.0 OK' "$TMPDIR/swaks-4.out" \
    && pass "dot-stuffed body delivered" \
    || fail "dot-stuffed body — expected 250 OK"

# --------------------------------------------------
echo ""
# Note: bare-LF line endings are tested at the protocol level by
# TestSMTPSmugglingBareLFDot in smtp_rfc_test.go.

sleep 0.5

# --------------------------------------------------
echo ""
echo "=== Stopping server ==="
kill "$SERVER_PID" 2>/dev/null || true
wait "$SERVER_PID" 2>/dev/null || true

# --------------------------------------------------
echo ""
echo "=== Postcat verification ==="
"$TMPDIR/verify-postcat" "$POSTCAT_DIR"

EXPECTED_COUNT=4
ACTUAL_COUNT=$(find "$POSTCAT_DIR" -name '*.eml' 2>/dev/null | wc -l)
[ "$ACTUAL_COUNT" -eq "$EXPECTED_COUNT" ] \
    && pass "postcat file count: $ACTUAL_COUNT" \
    || fail "postcat file count: $ACTUAL_COUNT (expected $EXPECTED_COUNT)"

grep -q '^S <>$' "$POSTCAT_DIR"/*.eml \
    && pass "null sender written as S <>" \
    || fail "null sender missing"

grep -c '^R ' "$POSTCAT_DIR"/*.eml | grep -q ':2$' \
    && pass "multi-recipient has 2 R lines" \
    || fail "multi-recipient R lines wrong"

grep -qF '.leading dot' "$POSTCAT_DIR"/*.eml \
    && pass "dot-stuffed body preserved" \
    || fail "dot-stuffed body missing expected content"

grep -qi "panic\|fatal" "$TMPDIR/server.err" 2>/dev/null \
    && fail "server panicked" \
    || pass "server did not panic"

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
