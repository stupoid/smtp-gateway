#!/usr/bin/env bash
# Concurrency correctness + load benchmark for smtp-gateway.
set -euo pipefail
cd "$(dirname "$0")/.."
source "$(dirname "$0")/_e2e_tools.sh"

TMPDIR=$(mktemp -d)
POSTCAT_DIR="$TMPDIR/mail"
ADDR="127.0.0.1:12555"

cleanup() {
    # Kill any background swaks + server.
    jobs -p | xargs -r kill 2>/dev/null || true
    rm -rf "$TMPDIR"
}
trap cleanup EXIT

RED='\033[0;31m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
pass() { echo -e "${GREEN}PASS${NC} $*"; }
fail() { echo -e "${RED}FAIL${NC} $*"; exit 1; }
info() { echo -e "${CYAN}INFO${NC} $*"; }

echo "=== Building ==="
CGO_ENABLED=0 go build -o test-server ./cmd/test-server/

echo "=== Starting server ==="
./test-server "$ADDR" "$POSTCAT_DIR" > "$TMPDIR/server.out" 2> "$TMPDIR/server.err" &
SRV=$!
for i in $(seq 1 30); do
    grep -q LISTENING "$TMPDIR/server.out" 2>/dev/null && break
    sleep 0.1
done
grep -q LISTENING "$TMPDIR/server.out" || { echo "=== server output ==="; cat "$TMPDIR/server.out"; echo "=== server stderr ==="; cat "$TMPDIR/server.err"; fail "server did not start"; }
info "server ready on $ADDR"

###############################################################################
# 1. CONCURRENCY CORRECTNESS
###############################################################################
echo ""
echo "============================================="
echo "  CONCURRENCY CORRECTNESS (50 connections)"
echo "============================================="

CONCURRENT=50
info "firing $CONCURRENT concurrent swaks connections..."

START=$(date +%s.%N)
PIDS=()
for i in $(seq 1 $CONCURRENT); do
    (
        "$SWAKS" \
            --from "concurrent-$i@test.local" \
            --to "rcpt-$i@test.local" \
            --server "$ADDR" \
            --body "body-for-connection-$i-unique-payload" \
            --header-Subject "concurrency test $i" \
            > "$TMPDIR/swaks-$i.out" 2>&1
    ) &
    PIDS+=($!)
done
for pid in "${PIDS[@]}"; do wait "$pid" 2>/dev/null || true; done
END=$(date +%s.%N)
ELAPSED=$(awk "BEGIN {printf \"%.2f\", $END - $START}")
info "all $CONCURRENT connections completed in ${ELAPSED}s"

# Check for failures (4xx/5xx SMTP responses).
FAILS=0
for i in $(seq 1 $CONCURRENT); do
    if grep -E '<-  [45][0-9][0-9] ' "$TMPDIR/swaks-$i.out" 2>/dev/null; then
        echo "swaks-$i failed: $(grep '<-  [45]' "$TMPDIR/swaks-$i.out" | head -1)"
        FAILS=$((FAILS + 1))
    fi
done
[ "$FAILS" -eq 0 ] || fail "$FAILS / $CONCURRENT swaks connections had error responses"
pass "all $CONCURRENT connections succeeded (no SMTP errors)"

# Verify postcat count.
sleep 0.5
POSTCAT_COUNT=$(find "$POSTCAT_DIR" -name '*.eml' 2>/dev/null | wc -l)
info "postcat files: $POSTCAT_COUNT (expected $CONCURRENT)"
[ "$POSTCAT_COUNT" -eq "$CONCURRENT" ] || fail "expected $CONCURRENT postcat files, got $POSTCAT_COUNT"

# Verify no cross-contamination.
info "checking envelope/body consistency..."
MISMATCH=0
for f in "$POSTCAT_DIR"/*.eml; do
    SENDER=$(head -1 "$f" | sed 's/^S //')
    CONN_ID=$(echo "$SENDER" | sed -n 's/concurrent-\([0-9]*\)@.*/\1/p')
    if [ -z "$CONN_ID" ]; then
        echo "  WARN: could not parse sender from $(basename "$f"): $SENDER"
        MISMATCH=$((MISMATCH + 1))
        continue
    fi
    EXPECTED="body-for-connection-${CONN_ID}-unique-payload"
    if ! grep -qF "$EXPECTED" "$f"; then
        echo "  MISMATCH: $(basename "$f") sender=$SENDER missing expected body"
        MISMATCH=$((MISMATCH + 1))
    fi
done
[ "$MISMATCH" -eq 0 ] || fail "$MISMATCH cross-contamination mismatches"
pass "no cross-contamination — all $CONCURRENT messages have correct envelope/body"

# Verify unique senders.
UNIQUE_SENDERS=$(head -1 "$POSTCAT_DIR"/*.eml 2>/dev/null | grep '^S ' | sort | uniq | wc -l)
[ "$UNIQUE_SENDERS" -eq "$CONCURRENT" ] || fail "expected $CONCURRENT unique senders, got $UNIQUE_SENDERS"
pass "all $CONCURRENT senders are unique"

# Panic check.
if grep -qi "panic\|fatal" "$TMPDIR/server.err" 2>/dev/null; then
    echo "=== SERVER PANIC ==="
    cat "$TMPDIR/server.err"
    fail "server panicked"
fi
pass "server did not panic"

###############################################################################
# 2. LOAD / BENCHMARK
###############################################################################
echo ""
echo "============================================="
echo "  LOAD TEST / BENCHMARKS"
echo "============================================="

bench_sequential() {
    local n=$1 start end
    start=$(date +%s.%N)
    for i in $(seq 1 "$n"); do
        "$SWAKS" --from "bseq-$i@t" --to "bseq-$i@t" \
            --server "$ADDR" --body "bp-$i" \
            > /dev/null 2>&1 || true
    done
    end=$(date +%s.%N)
    echo "$start $end"
}

bench_concurrent() {
    local n=$1 start end pids=()
    start=$(date +%s.%N)
    for i in $(seq 1 "$n"); do
        ( "$SWAKS" --from "bconc-$i@t" --to "bconc-$i@t" \
            --server "$ADDR" --body "bp-$i" \
            > /dev/null 2>&1 ) &
        pids+=($!)
    done
    for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done
    end=$(date +%s.%N)
    echo "$start $end"
}

N_SEQ=50
info "sequential: $N_SEQ messages..."
read -r SS_SEQ ES_SEQ <<< "$(bench_sequential "$N_SEQ")"
SEQ_ELAPSED=$(awk "BEGIN {printf \"%.4f\", $ES_SEQ - $SS_SEQ}")
SEQ_RATE=$(awk "BEGIN {printf \"%.1f\", $N_SEQ / $SEQ_ELAPSED}")
printf "  %-20s %4d msgs in %6.2fs  ->  %7.1f msg/s\n" "sequential" "$N_SEQ" "$SEQ_ELAPSED" "$SEQ_RATE"

for LEVEL in 20 50 100 200; do
    read -r SS_CONC ES_CONC <<< "$(bench_concurrent "$LEVEL")"
    CONC_ELAPSED=$(awk "BEGIN {printf \"%.4f\", $ES_CONC - $SS_CONC}")
    CONC_RATE=$(awk "BEGIN {printf \"%.1f\", $LEVEL / $CONC_ELAPSED}")
    printf "  %-20s %4d msgs in %6.2fs  ->  %7.1f msg/s\n" "concurrent $LEVEL" "$LEVEL" "$CONC_ELAPSED" "$CONC_RATE"
done

echo ""
echo "Server stderr (last 10 lines):"
tail -10 "$TMPDIR/server.err" 2>/dev/null || echo "(none)"

kill "$SRV" 2>/dev/null || true
wait "$SRV" 2>/dev/null || true

echo ""
pass "ALL TESTS PASSED"
