#!/usr/bin/env bash
# Chaos test: starts the gateway, confirms it serves a real request,
# kills and restarts it under sustained k6 background traffic
# (loadtest/chaos.js), and confirms it serves a real request again
# afterward -- i.e. the process recovers from a mid-run restart rather
# than staying wedged.
#
# Providers in this codebase are in-process (see internal/providers),
# not separate services, so the README's "kill providers mid-run" is
# adapted here to "kill and restart the gateway process itself" -- the
# closest honest analog this architecture has. What this script does
# NOT verify: "no request... double-charged" would need reading back
# usage_ledger rows, which needs a live Postgres this environment
# doesn't have. See docs/adr/0015.
#
# This does NOT gate on k6's own aggregate success rate: while the
# gateway is down, a connection-refused failure returns in microseconds
# while a real success takes as long as the mock provider's configured
# latency, so a few unthrottled VUs produce a hugely disproportionate
# failure count during even a sub-second outage. k6 runs here only to
# generate realistic concurrent load across the restart; pass/fail comes
# from two direct probes instead. See loadtest/chaos.js's own comment.
#
# Usage: make chaos   (invokes this from the repo root)
set -euo pipefail

CONFIG="${1:-deploy/loadtest.config.yaml}"
export POSTGRES_DSN="${POSTGRES_DSN:-postgres://fake:fake@127.0.0.1:1/fakedb}"
GATEWAY_URL="${GATEWAY_URL:-http://127.0.0.1:18080}"
TOKEN="${LOADTEST_TOKEN:-sk-vk-loadtest}"
BIN="$(mktemp -u).exe"

cleanup() {
  if [ -n "${GW_PID:-}" ] && kill -0 "$GW_PID" 2>/dev/null; then
    kill -TERM "$GW_PID" 2>/dev/null || true
    wait "$GW_PID" 2>/dev/null || true
  fi
  rm -f "$BIN"
}
trap cleanup EXIT

echo "chaos: building gateway..."
go build -o "$BIN" ./cmd/gateway

wait_for_health() {
  for _ in $(seq 1 30); do
    if curl -s -o /dev/null "$GATEWAY_URL/healthz"; then
      return 0
    fi
    sleep 0.5
  done
  echo "chaos: gateway never became healthy at $GATEWAY_URL/healthz" >&2
  return 1
}

probe() {
  curl -s -o /dev/null -w '%{http_code}' -X POST "$GATEWAY_URL/v1/chat/completions" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -d '{"model":"fast","temperature":0.7,"messages":[{"role":"user","content":"chaos probe"}]}'
}

echo "chaos: starting gateway..."
"$BIN" --config "$CONFIG" &
GW_PID=$!
wait_for_health

echo "chaos: confirming the gateway serves a real request before any disruption..."
before_status="$(probe)"
if [ "$before_status" != "200" ]; then
  echo "chaos: FAILED -- baseline request before the kill returned $before_status, want 200"
  exit 1
fi
echo "chaos: baseline OK (200)"

echo "chaos: starting k6 background load (generates concurrent traffic across the restart)..."
k6 run loadtest/chaos.js &
K6_PID=$!

sleep 8
echo "chaos: killing gateway (SIGTERM, graceful shutdown) mid-run..."
kill -TERM "$GW_PID"
wait "$GW_PID" || true
unset GW_PID

echo "chaos: restarting gateway..."
"$BIN" --config "$CONFIG" &
GW_PID=$!
wait_for_health

echo "chaos: confirming the gateway serves a real request again after the restart..."
after_status="$(probe)"

set +e
wait "$K6_PID"
set -e

if [ "$after_status" != "200" ]; then
  echo "chaos: FAILED -- request after the restart returned $after_status, want 200"
  exit 1
fi
echo "chaos: passed -- gateway served a real request before the kill and again after the restart"
