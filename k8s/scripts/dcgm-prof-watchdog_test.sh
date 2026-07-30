#!/usr/bin/env bash
# Behavior tests for dcgm-prof-watchdog.sh using PATH-stubbed curl/kubectl.
# No external test framework: exits non-zero if any case fails.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/dcgm-prof-watchdog.sh"
fail=0

setup() {
  TMP="$(mktemp -d)"
  BIN="$TMP/bin"; mkdir -p "$BIN"
  CALLS="$TMP/kubectl_calls"; : > "$CALLS"
  BODY="$TMP/body"; : > "$BODY"
  SEQ="$TMP/seq"; mkdir -p "$SEQ"
  # stub curl: honor -o <file>. Two modes:
  #   STUB_SEQ_DIR set -> per-call body/code from $SEQ/<n>.body / <n>.code (n starts at 0)
  #   else            -> single STUB_BODY file + STUB_CODE
  cat > "$BIN/curl" <<'CURL'
#!/usr/bin/env bash
out=""
while [ $# -gt 0 ]; do case "$1" in -o) out="$2"; shift 2;; *) shift;; esac; done
if [ -n "${STUB_SEQ_DIR:-}" ]; then
  cf="$STUB_SEQ_DIR/.n"; n=0; [ -f "$cf" ] && n="$(cat "$cf")"; echo $((n+1)) > "$cf"
  if [ -n "$out" ]; then if [ -f "$STUB_SEQ_DIR/$n.body" ]; then cp "$STUB_SEQ_DIR/$n.body" "$out"; else : > "$out"; fi; fi
  if [ -f "$STUB_SEQ_DIR/$n.code" ]; then cat "$STUB_SEQ_DIR/$n.code"; else printf '200'; fi
else
  [ -n "$out" ] && cp "${STUB_BODY:-/dev/null}" "$out"
  printf '%s' "${STUB_CODE:-200}"
fi
CURL
  # stub kubectl: record each invocation
  cat > "$BIN/kubectl" <<KUBECTL
#!/usr/bin/env bash
echo "\$@" >> "$CALLS"
KUBECTL
  chmod +x "$BIN/curl" "$BIN/kubectl"
}
teardown() { rm -rf "$TMP"; }
calls() { wc -l < "$CALLS" | tr -d ' '; }
prof_line() { printf 'DCGM_FI_PROF_PCIE_TX_BYTES{gpu="0"} 123\n'; }
check() { # name expected actual
  if [ "$2" = "$3" ]; then echo "PASS $1"; else echo "FAIL $1 (expected $2, got $3)"; fail=1; fi
}

# A: endpoint 200 with PROF present -> never restarts
setup
prof_line > "$BODY"
env HOST_CMD="" PATH="$BIN:$PATH" NODE_NAME=n POLL_SECONDS=0 COOLDOWN_SECONDS=0 \
    STUB_BODY="$BODY" STUB_CODE=200 WATCHDOG_MAX_LOOPS=5 MISS_THRESHOLD=3 \
    bash "$SCRIPT" >/dev/null 2>&1
check A 0 "$(calls)"; teardown

# B: endpoint down (non-200) -> never restarts, miss never accrues
setup
: > "$BODY"
env HOST_CMD="" PATH="$BIN:$PATH" NODE_NAME=n POLL_SECONDS=0 COOLDOWN_SECONDS=0 \
    STUB_BODY="$BODY" STUB_CODE=503 WATCHDOG_MAX_LOOPS=6 MISS_THRESHOLD=3 \
    bash "$SCRIPT" >/dev/null 2>&1
check B 0 "$(calls)"; teardown

# C: endpoint 200 but PROF absent for MISS_THRESHOLD polls -> exactly one restart
setup
: > "$BODY"   # 200, zero PROF lines
env HOST_CMD="" PATH="$BIN:$PATH" NODE_NAME=n POLL_SECONDS=0 COOLDOWN_SECONDS=0 \
    STUB_BODY="$BODY" STUB_CODE=200 WATCHDOG_MAX_LOOPS=3 MISS_THRESHOLD=3 MAX_PER_HOUR=6 \
    bash "$SCRIPT" >/dev/null 2>&1
check C 1 "$(calls)"; teardown

# D: per-hour cap limits restarts (threshold 1, cap 2, 6 loops -> 2 restarts)
setup
: > "$BODY"
env HOST_CMD="" PATH="$BIN:$PATH" NODE_NAME=n POLL_SECONDS=0 COOLDOWN_SECONDS=0 \
    STUB_BODY="$BODY" STUB_CODE=200 WATCHDOG_MAX_LOOPS=6 MISS_THRESHOLD=1 MAX_PER_HOUR=2 \
    bash "$SCRIPT" >/dev/null 2>&1
check D 2 "$(calls)"; teardown

# E: PROF recovers before threshold -> miss resets, no restart
setup
: > "$SEQ/0.body"                 # absent
: > "$SEQ/1.body"                 # absent
prof_line > "$SEQ/2.body"         # present -> reset
: > "$SEQ/3.body"                 # absent (miss back to 1)
env HOST_CMD="" PATH="$BIN:$PATH" NODE_NAME=n POLL_SECONDS=0 COOLDOWN_SECONDS=0 \
    STUB_SEQ_DIR="$SEQ" WATCHDOG_MAX_LOOPS=4 MISS_THRESHOLD=3 \
    bash "$SCRIPT" >/dev/null 2>&1
check E 0 "$(calls)"; teardown

if [ "$fail" -ne 0 ]; then echo "TESTS FAILED"; exit 1; fi
echo "ALL TESTS PASSED"
