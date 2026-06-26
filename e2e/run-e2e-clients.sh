#!/usr/bin/env bash
# Test SMTP gateway against common CLI mail clients:
#   mutt, msmtp, curl (smtp://), swaks (baseline)
set -euo pipefail
cd "$(dirname "$0")/.."
source "$(dirname "$0")/_e2e_tools.sh"

TMPDIR=$(mktemp -d)
POSTCAT_DIR="$TMPDIR/mail"
ADDR="127.0.0.1:12541"
# Kill any leftover.
ss -tlnp "sport = :12541" 2>/dev/null | awk '/LISTEN/{gsub(/.*pid=/,""); gsub(/,.*/,""); print}' | xargs -r kill 2>/dev/null || true
sleep 0.3
cleanup() { rm -rf "$TMPDIR"; }
trap cleanup EXIT

RED='\033[0;31m'
GREEN='\033[0;32m'
RST='\033[0m'

pass()  { echo -e "${GREEN}PASS${RST} $*"; }
fail()  { echo -e "${RED}FAIL${RST} $*"; exit 1; }

echo "=== Building ==="
CGO_ENABLED=0 go build -o "$TMPDIR/test-server" ./cmd/test-server/
CGO_ENABLED=0 go build -o "$TMPDIR/verify-postcat" ./cmd/verify-postcat/

echo "=== Starting server ==="
"$TMPDIR/test-server" "$ADDR" "$POSTCAT_DIR" > "$TMPDIR/server.out" 2> "$TMPDIR/server.err" &
SRV=$!
for i in $(seq 1 30); do
    grep -q LISTENING "$TMPDIR/server.out" 2>/dev/null && break
    sleep 0.1
done
grep -q LISTENING "$TMPDIR/server.out" || fail "server did not start"
echo "Server ready on $ADDR"

HOST="${ADDR%:*}"
PORT="${ADDR#*:}"

# --------------------------------------------------
echo ""
echo "========== mutt =========="
echo "body via mutt" | "$MUTT" \
    -e "set smtp_url=smtp://$ADDR" \
    -e "set ssl_starttls=no" \
    -e "set ssl_force_tls=no" \
    -e "set from=mutt@client.local" \
    -e "set use_envelope_from=yes" \
    -s "mutt test" \
    -- mutt-recip@example.com 2>&1 || fail "mutt failed"
echo "mutt sent OK"

# --------------------------------------------------
echo ""
echo "========== msmtp =========="
printf "Subject: msmtp test\r\n\r\nbody via msmtp\r\n" | \
    "$MSMTP" \
    --host="$HOST" --port="$PORT" \
    --from=msmtp@client.local \
    -- \
    msmtp-recip@example.com 2>&1 || fail "msmtp failed"
echo "msmtp sent OK"

# --------------------------------------------------
echo ""
echo "========== curl (smtp://) =========="
printf "From: curl@client.local\r\nTo: curl-recip@example.com\r\nSubject: curl test\r\n\r\nbody via curl\r\n" > "$TMPDIR/curl-body.txt"
"$CURL" \
    smtp://"$ADDR" \
    --mail-from curl@client.local \
    --mail-rcpt curl-recip@example.com \
    --upload-file "$TMPDIR/curl-body.txt" \
    -s 2>&1 || fail "curl failed"
echo "curl sent OK"

# --------------------------------------------------
echo ""
echo "========== swaks (null sender) =========="
"$SWAKS" \
    --from '<>' \
    --to null@bounce.local \
    --server "$ADDR" \
    --body "bounce message" \
    --header-Subject "null sender bounce" 2>&1 | tail -1 || fail "swaks null sender failed"
echo "swaks null sender OK"

# --------------------------------------------------
echo ""
echo "========== swaks (multiple rcpt) =========="
"$SWAKS" \
    --from multi@client.local \
    --to rcpt1@a.local,rcpt2@b.local,rcpt3@c.local \
    --server "$ADDR" \
    --body "multi-recipient" \
    --header-Subject "multi test" 2>&1 | tail -1 || fail "swaks multi failed"
echo "swaks multi OK"

# --------------------------------------------------
sleep 0.5
kill "$SRV" 2>/dev/null || true
wait "$SRV" 2>/dev/null || true

echo ""
echo "============================================="
echo "      Postcat files"
echo "============================================="
for f in "$POSTCAT_DIR"/*.eml; do
    echo "--- $(basename "$f") ---"
    cat "$f"
    echo ""
done

echo "============================================="
echo "      Verify with ParsePostcat"
echo "============================================="
"$TMPDIR/verify-postcat" "$POSTCAT_DIR"

# Check server didn't panic.
if grep -qi "panic\|fatal" "$TMPDIR/server.err" 2>/dev/null; then
    fail "server stderr has panic/fatal:\n$(cat "$TMPDIR/server.err")"
fi

# Quick assertions.
echo ""
echo "=== Assertions ==="
COUNT=$(ls "$POSTCAT_DIR"/*.eml 2>/dev/null | wc -l)
[ "$COUNT" -eq 5 ] || fail "expected 5 postcat files, got $COUNT"
pass "5 messages delivered"

# null sender
grep -q '^S <>$' "$POSTCAT_DIR"/*.eml && pass "null sender written as S <>" || fail "null sender missing"

# multi rcpt
grep -c '^R ' "$POSTCAT_DIR"/*.eml | grep -q ':3$' && pass "multi-recipient has 3 R lines" || fail "multi-recipient R lines wrong"

echo ""
pass "ALL TESTS PASSED"
