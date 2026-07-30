#!/usr/bin/env bash
# DCGM profiling-metric watchdog.
# Detects "dcgm-exporter up but DCGM_FI_PROF_* absent" on THIS node and restarts
# the local dcgm-exporter pod so DCGM re-establishes its profiling watch.
#
# Source of truth for the dcgm-prof-watchdog sidecar. This file is inlined
# verbatim into args of that sidecar in k8s/deploy.yaml and
# k8s/devops_deploy.cuda128.yaml — keep the three copies in sync.
set -uo pipefail

DCGM_PORT="${DCGM_PORT:-9400}"
DCGM_NS="${DCGM_NS:-monitoring}"
DCGM_LABEL="${DCGM_LABEL:-app.kubernetes.io/name=dcgm-exporter}"
KUBECONFIG_PATH="${KUBECONFIG_PATH:-/var/sichek/run/current/kubeconfig}"
NODE_NAME="${NODE_NAME:-}"
POLL_SECONDS="${POLL_SECONDS:-30}"
MISS_THRESHOLD="${MISS_THRESHOLD:-3}"
COOLDOWN_SECONDS="${COOLDOWN_SECONDS:-300}"
MAX_PER_HOUR="${MAX_PER_HOUR:-6}"
# HOST_CMD runs host binaries via nsenter in-cluster; tests set it empty.
HOST_CMD="${HOST_CMD:-nsenter -t 1 -m -p -n -u -i --}"
# WATCHDOG_MAX_LOOPS bounds the loop for tests; empty = run forever.
WATCHDOG_MAX_LOOPS="${WATCHDOG_MAX_LOOPS:-}"

log() { echo "$(date '+%Y-%m-%dT%H:%M:%S%z') [dcgm-prof-watchdog] $*"; }

_body="$(mktemp)"
trap 'rm -f "$_body"' EXIT

fetch_code() {
  # writes body to $_body, prints HTTP code (000 if curl could not run)
  local code
  code="$($HOST_CMD curl -s -m 4 -o "$_body" -w '%{http_code}' \
    "http://localhost:${DCGM_PORT}/metrics" 2>/dev/null)"
  echo "${code:-000}"
}

prof_count() {
  local c
  c="$(grep -c '^DCGM_FI_PROF_' "$_body" 2>/dev/null)"
  echo "${c:-0}"
}

restart_dcgm() {
  $HOST_CMD kubectl --kubeconfig="${KUBECONFIG_PATH}" delete pod \
    -n "${DCGM_NS}" -l "${DCGM_LABEL}" \
    --field-selector "spec.nodeName=${NODE_NAME}"
}

# sliding 1h window of restart epoch seconds
restart_times=()
recent_restarts() {
  local now t c=0 keep=()
  now="$(date +%s)"
  for t in "${restart_times[@]:-}"; do
    if [ -n "$t" ] && [ $((now - t)) -lt 3600 ]; then keep+=("$t"); c=$((c + 1)); fi
  done
  restart_times=("${keep[@]:-}")
  echo "$c"
}

log "starting: node=${NODE_NAME} port=${DCGM_PORT} poll=${POLL_SECONDS}s miss=${MISS_THRESHOLD} cooldown=${COOLDOWN_SECONDS}s cap=${MAX_PER_HOUR}/h"
miss=0
loops=0
while true; do
  code="$(fetch_code)"
  if [ "$code" != "200" ]; then
    log "dcgm-exporter endpoint http=${code}; treating as down (skip), miss reset"
    miss=0
  else
    pc="$(prof_count)"
    if [ "$pc" -eq 0 ]; then
      miss=$((miss + 1))
      log "DCGM_FI_PROF_* absent (miss ${miss}/${MISS_THRESHOLD})"
    else
      [ "$miss" -gt 0 ] && log "DCGM_FI_PROF_* present again (${pc} lines), miss reset"
      miss=0
    fi
    if [ "$miss" -ge "$MISS_THRESHOLD" ]; then
      n="$(recent_restarts)"
      if [ "$n" -ge "$MAX_PER_HOUR" ]; then
        log "WARN per-hour restart cap reached (${n}/${MAX_PER_HOUR}); NOT restarting"
      else
        log "restarting dcgm-exporter pod on ${NODE_NAME} (miss=${miss})"
        if restart_dcgm; then
          restart_times+=("$(date +%s)")
          miss=0
          log "restart issued; cooldown ${COOLDOWN_SECONDS}s"
          sleep "$COOLDOWN_SECONDS"
        else
          log "WARN kubectl delete failed; will retry next cycle"
        fi
      fi
    fi
  fi
  loops=$((loops + 1))
  if [ -n "$WATCHDOG_MAX_LOOPS" ] && [ "$loops" -ge "$WATCHDOG_MAX_LOOPS" ]; then
    log "reached WATCHDOG_MAX_LOOPS=${WATCHDOG_MAX_LOOPS}, exiting"
    break
  fi
  sleep "$POLL_SECONDS"
done
