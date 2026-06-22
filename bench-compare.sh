#!/usr/bin/env bash
# Benchmark smtp-gateway vs go-smtp library.
# Runs the same concurrency/throughput load tests against both servers
# and prints a comparison table.
set -euo pipefail
cd "$(dirname "$0")"
source "$(dirname "$0")/_e2e_tools.sh"

TMPDIR=$(mktemp -d -t smtp-bench-XXXXXX)
cleanup() { jobs -p | xargs -r kill 2>/dev/null || true; rm -rf "$TMPDIR"; }
trap cleanup EXIT

RED='\033[0;31m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
pass()  { echo -e "${GREEN}PASS${NC} $*"; }
fail()  { echo -e "${RED}FAIL${NC} $*"; exit 1; }
info()  { echo -e "${CYAN}INFO${NC} $*"; }

echo "=== Building servers ==="
CGO_ENABLED=0 go build -o "$TMPDIR/test-server" ./cmd/test-server/
( cd bench-go-smtp && CGO_ENABLED=0 go build -o "$TMPDIR/go-smtp-server" . )
[ -x "$TMPDIR/test-server" ] || fail "test-server build failed"
[ -x "$TMPDIR/go-smtp-server" ] || fail "go-smtp-server build failed"

###############################################################################
# run_bench <name> <binary> <port>
###############################################################################
run_bench() {
    local name="$1" bin="$2" port="$3"
    local addr="127.0.0.1:$port"
    local postcat="$TMPDIR/postcat-$name"
    mkdir -p "$postcat"

    echo ""
    echo "============================================="
    info "SERVER: $name ($addr)"
    echo "============================================="

    # Start server.
    "$bin" "$addr" "$postcat" > "$TMPDIR/server-$name.out" 2> "$TMPDIR/server-$name.err" &
    local srv=$!
    for i in $(seq 1 30); do
        grep -q LISTENING "$TMPDIR/server-$name.out" 2>/dev/null && break
        sleep 0.1
    done
    grep -q LISTENING "$TMPDIR/server-$name.out" || { fail "$name did not start"; cat "$TMPDIR/server-$name.err"; }

    # ---- Concurrency correctness (20 connections) ----
    local n=20 pids=()
    for i in $(seq 1 $n); do
        ( "$SWAKS" --from "corr-$i@t" --to "r$i@t" --server "$addr" \
          --body "uid-body-$i" > "$TMPDIR/corr-$name-$i.out" 2>&1 ) &
        pids+=($!)
    done
    for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done
    local fails=0
    for i in $(seq 1 $n); do
        grep -E '<-  [45][0-9][0-9] ' "$TMPDIR/corr-$name-$i.out" 2>/dev/null && fails=$((fails+1))
    done
    local pc=$(find "$postcat" -name '*.eml' 2>/dev/null | wc -l)
    [ "$fails" -eq 0 ] || echo "  WARN: $fails/$n error responses"
    [ "$pc" -eq "$n" ] || echo "  WARN: expected $n postcat files, got $pc"
    local mismatch=0
    for f in "$postcat"/*.eml; do
        local sid=$(head -1 "$f" 2>/dev/null | sed 's/^S //')
        local cid=$(echo "$sid" | sed -n 's/^corr-\([0-9]*\)@.*/\1/p')
        [ -z "$cid" ] && continue
        grep -qF "uid-body-$cid" "$f" || { mismatch=$((mismatch+1)); break; }
    done
    [ "$mismatch" -eq 0 ] && pass "correctness OK ($n/$n)" || echo "  WARN: $mismatch cross-contam"

    # ---- Throughput benchmarks ----
    bench_seq() {
        local m=$1 t0 t1
        t0=$(date +%s.%N)
        for i in $(seq 1 "$m"); do
            "$SWAKS" --from "s$i@t" --to "r$i@t" --server "$addr" --body "bp$i" > /dev/null 2>&1 || true
        done
        t1=$(date +%s.%N)
        awk "BEGIN {printf \"%.4f\", $t1 - $t0}"
    }
    bench_conc() {
        local m=$1 t0 t1 pids=()
        t0=$(date +%s.%N)
        for i in $(seq 1 "$m"); do
            ( "$SWAKS" --from "c$i@t" --to "r$i@t" --server "$addr" --body "bp$i" > /dev/null 2>&1 ) &
            pids+=($!)
        done
        for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done
        t1=$(date +%s.%N)
        awk "BEGIN {printf \"%.4f\", $t1 - $t0}"
    }

    # Sequential: 20
    local seq_t=$(bench_seq 20)
    local seq_r=$(awk "BEGIN {printf \"%.1f\", 20 / $seq_t}")
    printf "  %-20s %2d msgs in %6.2fs  ->  %7.1f msg/s\n" "sequential" 20 "$seq_t" "$seq_r"

    # Concurrent at various levels
    for level in 20 50 100; do
        local conc_t=$(bench_conc "$level")
        local conc_r=$(awk "BEGIN {printf \"%.1f\", $level / $conc_t}")
        printf "  %-20s %2d msgs in %6.2fs  ->  %7.1f msg/s\n" "concurrent $level" "$level" "$conc_t" "$conc_r"
    done

    # Panic check
    if grep -qi "panic\|fatal" "$TMPDIR/server-$name.err" 2>/dev/null; then
        fail "$name panicked!"
    fi

    kill "$srv" 2>/dev/null || true
    wait "$srv" 2>/dev/null || true
}

###############################################################################
# Run both benchmarks
###############################################################################
START_ALL=$(date +%s)
run_bench "smtp-gateway" "$TMPDIR/test-server" 12600
run_bench "go-smtp"       "$TMPDIR/go-smtp-server" 12601
END_ALL=$(date +%s)

echo ""
echo "============================================="
echo "  SUMMARY"
echo "============================================="
echo "Total wall time: $((END_ALL - START_ALL))s"
echo ""
echo "Server stderr (last 5 lines each):"
echo "--- smtp-gateway ---"
tail -5 "$TMPDIR/server-smtp-gateway.err" 2>/dev/null || echo "(none)"
echo "--- go-smtp ---"
tail -5 "$TMPDIR/server-go-smtp.err" 2>/dev/null || echo "(none)"
