#!/usr/bin/env bash

set -u

strip_ansi() {
  printf '%s' "${1:-}" | sed $'s/\033\\[[0-9;]*[A-Za-z]//g; s/\033.//g'
}

json_ok() {
  local msg
  msg="$(strip_ansi "$1")"
  printf '__SICORE_OUTPUT__=%s\n' \
    "$(jq -cn --arg message "$msg" '{"code":0,"status":true,"message":$message}')"
  exit 0
}

json_fail() {
  local msg
  msg="$(strip_ansi "$1")"
  printf '__SICORE_OUTPUT__=%s\n' \
    "$(jq -cn --arg message "$msg" '{"code":400,"status":false,"message":$message}')"
  exit 255
}

SICHEK_BIN="${SICHEK_BIN:-sichek}"

if ! command -v "$SICHEK_BIN" >/dev/null 2>&1; then
  json_fail "sichek binary not found: $SICHEK_BIN"
fi

if ! command -v nvidia-smi >/dev/null 2>&1; then
  json_ok "no nvidia GPU present; sichek g bypassed"
fi

output="$("$SICHEK_BIN" g -v 2>&1)"
rc=$?

filtered="$(printf '%s\n' "$output" | grep -iv 'terminal size\|ioctl for device\|kubelet socket does not exist')"

ok_summary="$(printf '%s\n' "$filtered" \
  | grep -Ei 'PASS|FAIL|ERROR|WARN|Result|Status' \
  | tr -s ' ' \
  | tail -n 20 \
  | tr '\n' ';')"
[[ -n "$ok_summary" ]] || ok_summary="$(printf '%s' "$filtered" | tr -s ' ' | tail -c 512)"

fail_summary="$(printf '%s' "$filtered" | tr -s ' ' | tail -c 2048 | tr '\n' ';')"
[[ -n "$fail_summary" ]] || fail_summary="$ok_summary"

if [[ $rc -ne 0 ]]; then
  json_fail "sichek g failed (rc=$rc): ${fail_summary}"
fi

if printf '%s\n' "$filtered" | grep -Eiq '\bFAIL\b|\bERROR\b'; then
  json_fail "sichek g reported failures: ${fail_summary}"
fi

json_ok "sichek g passed; ${ok_summary}"
