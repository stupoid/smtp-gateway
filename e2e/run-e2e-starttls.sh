#!/usr/bin/env bash
# STARTTLS e2e test: generate a self-signed cert, start the test server with
# TLS enabled, and deliver a message over STARTTLS.
set -euo pipefail
cd "$(dirname "$0")/.."
source "$(dirname "$0")/_e2e_tools.sh"

TMPDIR=$(mktemp -d)
POSTCAT_DIR="$TMPDIR/mail"
SERVER_ADDR="127.0.0.1:12530"
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

echo "=== Generating self-signed cert ==="
cat > "$TMPDIR/gencert.go" <<'GOSCRIPT'
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"time"
)

func main() {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{"localhost"},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certF, _ := os.Create(os.Args[1])
	pem.Encode(certF, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	certF.Close()
	keyF, _ := os.Create(os.Args[2])
	b, _ := x509.MarshalECPrivateKey(key)
	pem.Encode(keyF, &pem.Block{Type: "EC PRIVATE KEY", Bytes: b})
	keyF.Close()
}
GOSCRIPT
go run "$TMPDIR/gencert.go" "$TMPDIR/cert.pem" "$TMPDIR/key.pem" 2>&1 || { fail "cert generation failed"; exit 1; }

echo "=== Starting server with STARTTLS ==="
"$TMPDIR/test-server" -tls-cert "$TMPDIR/cert.pem" -tls-key "$TMPDIR/key.pem" \
    "$SERVER_ADDR" "$POSTCAT_DIR" > "$TMPDIR/server.out" 2> "$TMPDIR/server.err" &
SERVER_PID=$!
for i in $(seq 1 30); do
    if grep -q LISTENING "$TMPDIR/server.out" 2>/dev/null; then break; fi
    sleep 0.1
done
grep -q LISTENING "$TMPDIR/server.out" || { fail "server did not start"; cat "$TMPDIR/server.err"; exit 1; }
echo "Server ready."

# --------------------------------------------------
echo ""
echo "=== Test 1: deliver message over STARTTLS ==="
"$SWAKS" \
    --tls \
    --to tlsuser@example.com \
    --from tls@example.net \
    --server "$SERVER_ADDR" \
    --body "delivered over STARTTLS" \
    --header-Subject "STARTTLS test" \
    > "$TMPDIR/swaks-tls.out" 2>&1 || true
if grep -q '250 2.0.0 OK' "$TMPDIR/swaks-tls.out"; then
    pass "STARTTLS message delivered"
else
    fail "STARTTLS — expected 250 OK"; cat "$TMPDIR/swaks-tls.out"
fi

# --------------------------------------------------
echo ""
echo "=== Test 2: STARTTLS is required ==="
# Without --tls, MAIL FROM should get 530.
"$SWAKS" \
    --to plain@example.com \
    --from plain@example.net \
    --server "$SERVER_ADDR" \
    --quit-after MAIL \
    > "$TMPDIR/swaks-plain.out" 2>&1 || true
grep -qE '<- 530|<\*\* 530' "$TMPDIR/swaks-plain.out" \
    && pass "plain-text MAIL rejected with 530 (STARTTLS required)" \
    || { fail "plain-text MAIL — expected 530 STARTTLS required"; cat "$TMPDIR/swaks-plain.out"; }

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
[ "$COUNT" -eq 1 ] \
    && pass "exactly 1 postcat file (TLS only)" \
    || fail "expected 1 postcat file, got $COUNT"

grep -qi "panic\|fatal" "$TMPDIR/server.err" 2>/dev/null \
    && fail "server panicked" \
    || pass "server did not panic"

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
