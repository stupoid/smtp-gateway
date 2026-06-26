#!/usr/bin/env bash
# Error handling e2e tests: bad command sequence, unknown commands, bad syntax,
# RSET, and VRFY.  Asserts specific SMTP response codes.
set -euo pipefail
cd "$(dirname "$0")/.."
source "$(dirname "$0")/_e2e_tools.sh"

TMPDIR=$(mktemp -d)
POSTCAT_DIR="$TMPDIR/mail"
SERVER_ADDR="127.0.0.1:12528"
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

echo "=== Starting server ==="
"$TMPDIR/test-server" "$SERVER_ADDR" "$POSTCAT_DIR" > "$TMPDIR/server.out" 2> "$TMPDIR/server.err" &
SERVER_PID=$!
for i in $(seq 1 30); do
    if grep -q LISTENING "$TMPDIR/server.out" 2>/dev/null; then break; fi
    sleep 0.1
done
grep -q LISTENING "$TMPDIR/server.out" || { fail "server did not start"; exit 1; }
echo "Server ready."

# assert_response <label> <want-code> <output>
assert_response() {
    local label="$1" want="$2" output="$3"
    if grep -q "$want" "$output"; then
        pass "$label — got $want"
    else
        fail "$label — expected $want, got: $(head -3 "$output")"
    fi
}

# --------------------------------------------------
echo ""
echo "=== Test 1: RCPT before HELO ==="
printf 'RCPT TO:<x@y>\r\nQUIT\r\n' | "$NC" -w 2 127.0.0.1 12528 > "$TMPDIR/nc-1.out" 2>&1
assert_response "RCPT before HELO" "503" "$TMPDIR/nc-1.out"

echo ""
echo "=== Test 2: DATA before RCPT ==="
printf 'EHLO test\r\nMAIL FROM:<a@b>\r\nDATA\r\nQUIT\r\n' | "$NC" -w 2 127.0.0.1 12528 > "$TMPDIR/nc-2.out" 2>&1
assert_response "DATA before RCPT" "503" "$TMPDIR/nc-2.out"

echo ""
echo "=== Test 3: unknown command ==="
printf 'EHLO test\r\nFOOBAR\r\nQUIT\r\n' | "$NC" -w 2 127.0.0.1 12528 > "$TMPDIR/nc-3.out" 2>&1
assert_response "unknown command" "500" "$TMPDIR/nc-3.out"

echo ""
echo "=== Test 4: MAIL FROM with no angle brackets ==="
printf 'EHLO test\r\nMAIL FROM: bare\r\nQUIT\r\n' | "$NC" -w 2 127.0.0.1 12528 > "$TMPDIR/nc-4.out" 2>&1
assert_response "MAIL FROM bare" "501" "$TMPDIR/nc-4.out"

echo ""
echo "=== Test 5: RSET clears transaction state ==="
# RSET after MAIL+RCPT resets; a new MAIL FROM must succeed.
printf 'EHLO rs\r\nMAIL FROM:<rset@test>\r\nRCPT TO:<a@x>\r\nRSET\r\nMAIL FROM:<after@test>\r\nRCPT TO:<b@x>\r\nDATA\r\nSubject: rset\r\n\r\nrset body\r\n.\r\nQUIT\r\n' \
    | "$NC" -w 3 127.0.0.1 12528 > "$TMPDIR/nc-5.out" 2>&1
grep -q '250 2.0.0 OK' "$TMPDIR/nc-5.out" \
    && pass "RSET — message delivered after reset" \
    || fail "RSET — expected 250 OK after reset"

echo ""
echo "=== Test 6: VRFY returns 252 (disabled) ==="
printf 'EHLO vrfy\r\nVRFY test\r\nQUIT\r\n' | "$NC" -w 2 127.0.0.1 12528 > "$TMPDIR/nc-6.out" 2>&1
assert_response "VRFY disabled" "252" "$TMPDIR/nc-6.out"

# --------------------------------------------------
sleep 0.2
kill "$SERVER_PID" 2>/dev/null || true
wait "$SERVER_PID" 2>/dev/null || true

echo ""
grep -qi "panic\|fatal" "$TMPDIR/server.err" 2>/dev/null \
    && fail "server panicked" \
    || pass "server did not panic"

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
