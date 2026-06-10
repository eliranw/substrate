#!/usr/bin/env bash
#
# atefleet-dashboard.sh — serve the live atefleet dashboard and continuously
# feed it REAL data from the valkey index.
#
# Run this ONCE in a side terminal; then run `atefleet …` commands one-by-one
# in another terminal (see hack/poc/DEMO.md). The dashboard auto-refreshes:
#   - every 1s it runs `atefleet ls -o json` (read path: SMEMBERS atefleet:actor-ids
#     → GET atefleet:actor:<id>, ⊕ live status from ateapi) → fleet.json
#   - sub-task runs appear when you use `hack/poc/arun -- <cmd>` (it logs to runs.json)
#
# ENV: ATEFLEET (default bin/atefleet), FLEET_ADDR (default localhost:18443),
#      DEMO_DIR (default /tmp/atefleet-demo), HTTP_PORT (default 8899), INTERVAL (1)
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ATEFLEET="${ATEFLEET:-$ROOT/bin/atefleet}"
FLEET_ADDR="${FLEET_ADDR:-localhost:18443}"
DEMO_DIR="${DEMO_DIR:-/tmp/atefleet-demo}"
HTTP_PORT="${HTTP_PORT:-8899}"
INTERVAL="${INTERVAL:-1}"

[ -x "$ATEFLEET" ] || { echo "atefleet not found at $ATEFLEET — build: go build -o bin/atefleet ./cmd/atefleet" >&2; exit 1; }
"$ATEFLEET" --fleet-addr "$FLEET_ADDR" ls >/dev/null 2>&1 || {
  echo "can't reach FleetManager at $FLEET_ADDR. Port-forward it first:" >&2
  echo "  KUBECONFIG=~/.kube/atefleet-kind.config kubectl port-forward -n ate-system svc/atefleet 18443:443" >&2
  exit 1; }

mkdir -p "$DEMO_DIR"
cp "$ROOT/hack/poc/atefleet-demo-dashboard.html" "$DEMO_DIR/index.html"
[ -f "$DEMO_DIR/runs.json" ] || echo '[]' > "$DEMO_DIR/runs.json"
echo '[]' > "$DEMO_DIR/runs.json"   # fresh run log each session
echo '[]' > "$DEMO_DIR/fleet.json"

pkill -f "http.server $HTTP_PORT" 2>/dev/null || true; sleep 0.3
( cd "$DEMO_DIR" && exec python3 -m http.server "$HTTP_PORT" >/dev/null 2>&1 ) &
SRV=$!
cleanup(){ kill "$SRV" 2>/dev/null; echo; echo "dashboard stopped."; exit 0; }
trap cleanup INT TERM

URL="http://localhost:$HTTP_PORT/"
echo "atefleet dashboard live at:  $URL"
echo "feeding fleet.json from: $ATEFLEET --fleet-addr $FLEET_ADDR ls -o json  (every ${INTERVAL}s)"
echo "run sub-tasks with: hack/poc/arun -- <cmd>   (so they show in the runs panel)"
echo "Ctrl-C to stop."
command -v open >/dev/null && open "$URL" >/dev/null 2>&1 || true

while true; do
  "$ATEFLEET" --fleet-addr "$FLEET_ADDR" ls -o json > "$DEMO_DIR/fleet.json.tmp" 2>/dev/null \
    && mv "$DEMO_DIR/fleet.json.tmp" "$DEMO_DIR/fleet.json" || echo '[]' > "$DEMO_DIR/fleet.json"
  sleep "$INTERVAL"
done
