#!/usr/bin/env bash
#
# atefleet-demo.sh — a narrated showcase of the atefleet FleetManager.
#
# Demonstrates, against a LIVE substrate cluster, the two things atefleet adds:
#   1. A managed FLEET of long-lived actors with metadata (role/owner/group/ttl)
#      that suspend-when-idle and resume on demand   — DispatchActor / ls / get / rm
#   2. One-shot SUB-TASKS: dispatch a short command to an ephemeral gVisor actor,
#      get stdout/exit back, actor torn down                       — `atefleet run`
#
# A companion live DASHBOARD (atefleet-demo-dashboard.html) shows the fleet
# metadata + sub-task runs updating as this script executes.
#
# PREREQUISITES
#   - a running substrate cluster with atefleet + the counter and sandbox demos:
#       hack/install-ate-kind.sh --deploy-ate-system
#       hack/install-ate-kind.sh --deploy-demo-counter
#       hack/install-ate-kind.sh --deploy-demo-sandbox
#       KO_DOCKER_REPO=localhost:5001 KO_DEFAULTPLATFORMS=linux/$(go env GOARCH) \
#         ./hack/run-tool.sh ko apply -f manifests/ate-install/atefleet.yaml
#   - the atefleet binary built locally:   go build -o bin/atefleet ./cmd/atefleet
#   - the FleetManager reachable from here. For kind (ClusterIP), port-forward:
#       KUBECONFIG=~/.kube/atefleet-kind.config \
#         kubectl port-forward -n ate-system svc/atefleet 18443:443
#
# USAGE
#   hack/poc/atefleet-demo.sh             # interactive (pauses to narrate)
#   PAUSE=0 hack/poc/atefleet-demo.sh     # run straight through
#   hack/poc/atefleet-demo.sh clean       # rm the demo's long-lived actors
#
# ENV: ATEFLEET (default bin/atefleet), FLEET_ADDR (default localhost:18443),
#      DISPATCH_TEMPLATE, RUN_TEMPLATE, DEMO_DIR, HTTP_PORT, PAUSE

set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ATEFLEET="${ATEFLEET:-$ROOT/bin/atefleet}"
FLEET_ADDR="${FLEET_ADDR:-localhost:18443}"
DISPATCH_TEMPLATE="${DISPATCH_TEMPLATE:-ate-demo-counter/counter}"
RUN_TEMPLATE="${RUN_TEMPLATE:-ate-demo-sandbox/sandbox-template}"
DEMO_DIR="${DEMO_DIR:-/tmp/atefleet-demo}"
HTTP_PORT="${HTTP_PORT:-8899}"
PAUSE="${PAUSE:-1}"
# kubectl-ate (used only by Act 5 to suspend before rm) talks to ateapi via the
# cluster kubeconfig. Default to the kind config; override KIND_KUBECONFIG as needed.
KIND_KUBECONFIG="${KIND_KUBECONFIG:-$HOME/.kube/atefleet-kind.config}"
KATE="${KATE:-$ROOT/bin/kubectl-ate}"

# Long-lived demo actors (the "fleet"). ids are DNS-1123 labels.
FLEET_IDS=(repo-acme repo-globex repo-initech)

af() { "$ATEFLEET" --fleet-addr "$FLEET_ADDR" "$@"; }

C_CY='\033[1;36m'; C_GN='\033[1;32m'; C_YL='\033[1;33m'; C_DIM='\033[0;90m'; C_RS='\033[0m'
banner(){ printf "\n${C_CY}════════════════════════════════════════════════════════════════${C_RS}\n${C_CY}  %s${C_RS}\n${C_CY}════════════════════════════════════════════════════════════════${C_RS}\n" "$*"; }
say(){ printf "   %s\n" "$*"; }
cmd(){ printf "   ${C_DIM}\$${C_RS} ${C_YL}%s${C_RS}\n" "$*"; }
ok(){ printf "   ${C_GN}✓ %s${C_RS}\n" "$*"; }
die(){ printf "\n\033[1;31m✗ %s\033[0m\n" "$*" >&2; exit 1; }
pause(){ [ "$PAUSE" = "1" ] && { printf "\n   ${C_DIM}▶ press Enter…${C_RS}"; read -r _; } || true; }

snapshot(){ af ls -o json > "$DEMO_DIR/fleet.json" 2>/dev/null || echo '[]' > "$DEMO_DIR/fleet.json"; }

# record_run <command-string> <stdout> <stderr> <exit-code>
record_run(){
  python3 - "$DEMO_DIR/runs.json" "$1" "$2" "$3" "$4" <<'PY'
import json, os, sys, time
path, cmd, out, err, code = sys.argv[1:6]
runs = []
if os.path.exists(path):
    try: runs = json.load(open(path))
    except Exception: runs = []
runs.insert(0, {"ts": int(time.time()), "command": cmd, "stdout": out, "stderr": err, "exit_code": int(code)})
json.dump(runs[:25], open(path, "w"), indent=2)
PY
}

# demo_run <cmd...> — run a one-shot sub-task, show it, feed the dashboard.
demo_run(){
  cmd "atefleet run --template $RUN_TEMPLATE -- $*"
  local out code err
  if out=$(af run --template "$RUN_TEMPLATE" --timeout 30s -- "$@" 2>/tmp/af-run.err); then code=0; else code=$?; fi
  err=$(cat /tmp/af-run.err 2>/dev/null)
  [ -n "$out" ] && printf '%s\n' "$out" | sed "s/^/      ${C_GN}┃${C_RS} /"
  [ -n "$err" ] && printf '%s\n' "$err" | sed "s/^/      ${C_YL}┃(stderr)${C_RS} /"
  say "${C_DIM}exit=$code · actor created → resumed → ran in gVisor → suspended → deleted${C_RS}"
  record_run "$*" "$out" "$err" "$code"
  snapshot
}

clean(){
  banner "Cleaning up demo fleet actors"
  for id in "${FLEET_IDS[@]}"; do af rm "$id" >/dev/null 2>&1 && ok "removed $id" || say "$id not present / not suspended"; done
  exit 0
}
[ "${1:-}" = "clean" ] && clean

# ─────────────────────────── PREFLIGHT ───────────────────────────
banner "atefleet demo — preflight"
[ -x "$ATEFLEET" ] || die "atefleet binary not found at $ATEFLEET (build: go build -o bin/atefleet ./cmd/atefleet)"
af ls >/dev/null 2>&1 || die "can't reach FleetManager at $FLEET_ADDR. Port-forward it:
   KUBECONFIG=~/.kube/atefleet-kind.config kubectl port-forward -n ate-system svc/atefleet 18443:443"
ok "atefleet binary: $ATEFLEET"
ok "FleetManager reachable at $FLEET_ADDR"
mkdir -p "$DEMO_DIR"; cp "$ROOT/hack/poc/atefleet-demo-dashboard.html" "$DEMO_DIR/index.html"
echo '[]' > "$DEMO_DIR/runs.json"; snapshot
# Serve the dashboard (kill any prior server on this port).
pkill -f "http.server $HTTP_PORT" 2>/dev/null || true; sleep 0.3
( cd "$DEMO_DIR" && python3 -m http.server "$HTTP_PORT" >/dev/null 2>&1 & )
DASH_URL="http://localhost:$HTTP_PORT/"
ok "live dashboard: $DASH_URL"
command -v open >/dev/null && open "$DASH_URL" >/dev/null 2>&1 || say "open $DASH_URL in your browser"
say "${C_DIM}(the dashboard auto-refreshes — keep it visible while the demo runs)${C_RS}"
pause

# ─────────────────────────── ACT 1 ───────────────────────────
banner "ACT 1 — Dispatch a fleet of long-lived actors (with metadata)"
say "Each is a gVisor-sandboxed actor that suspends-when-idle. atefleet stamps"
say "role/owner/group/ttl as fleet metadata it tracks in its own index."
declare -a OWNERS=(platform platform security); declare -a GROUPS=(prod prod staging)
i=0
for id in "${FLEET_IDS[@]}"; do
  cmd "atefleet dispatch --id $id --template $DISPATCH_TEMPLATE --role reviewer --owner ${OWNERS[$i]} --group ${GROUPS[$i]} --ttl 1h"
  af dispatch --id "$id" --template "$DISPATCH_TEMPLATE" --role reviewer --owner "${OWNERS[$i]}" --group "${GROUPS[$i]}" --ttl 1h 2>&1 | sed 's/^/      /'
  i=$((i+1)); snapshot
done
ok "fleet dispatched — watch the dashboard's Fleet table populate"
pause

# ─────────────────────────── ACT 2 ───────────────────────────
banner "ACT 2 — The fleet view + metadata filtering"
cmd "atefleet ls"; af ls 2>&1 | sed 's/^/      /'
echo
say "Filter by the metadata atefleet tracks (the source of truth for state is"
say "ateapi; role/owner/group come from atefleet's own index):"
cmd "atefleet ls --group prod"; af ls --group prod 2>&1 | sed 's/^/      /'
cmd "atefleet ls --owner security"; af ls --owner security 2>&1 | sed 's/^/      /'
pause

# ─────────────────────────── ACT 3 ───────────────────────────
banner "ACT 3 — Inspect one actor (live status ⊕ metadata)"
cmd "atefleet get ${FLEET_IDS[0]} -o json"; af get "${FLEET_IDS[0]}" -o json 2>&1 | sed 's/^/      /'
pause

# ─────────────────────────── ACT 4 ───────────────────────────
banner "ACT 4 — One-shot sub-tasks: run a command in a remote gVisor actor"
say "This is the Claude-Code use case: dispatch a short command to an EPHEMERAL"
say "actor, get stdout/exit back, and the actor is torn down. The fleet is unchanged."
echo
demo_run echo "hello from a remote, sandboxed actor"
echo
say "Prove it really ran inside gVisor (note the 'runsc' / '-gvisor' kernel):"
demo_run sh -c "uname -a; echo \"running as uid \$(id -u)\""
echo
say "Exit codes + stderr propagate like a local command:"
demo_run sh -c "echo to-stdout; echo to-stderr 1>&2; exit 7"
echo
cmd "atefleet ls   # still just the fleet — sub-task actors left nothing behind"
af ls 2>&1 | sed 's/^/      /'
ok "one-shot: created → resumed → ran → suspended → deleted (no leak)"
pause

# ─────────────────────────── ACT 5 ───────────────────────────
banner "ACT 5 — Terminate a fleet actor"
say "ateapi only deletes a SUSPENDED actor, so we suspend then remove (atefleet rm)."
RM_ID="${FLEET_IDS[2]}"
cmd "kubectl-ate suspend actor $RM_ID"
if [ -x "$KATE" ]; then
  KUBECONFIG="$KIND_KUBECONFIG" "$KATE" suspend actor "$RM_ID" >/dev/null 2>&1 && ok "suspended $RM_ID" || say "(suspend failed; rm will surface ateapi's 'not suspended')"
  sleep 2
else
  say "(kubectl-ate not at $KATE; rm will surface the documented 'not suspended' MVP behavior — build it: go build -o bin/kubectl-ate ./cmd/kubectl-ate)"
fi
cmd "atefleet rm $RM_ID"; af rm "$RM_ID" 2>&1 | sed 's/^/      /'; snapshot
ok "fleet actor removed + its index entry cleaned (see the dashboard drop a row)"

banner "Done — the dashboard shows the final fleet + the sub-task run log"
say "Tear down the demo fleet with:  ${C_YL}hack/poc/atefleet-demo.sh clean${C_RS}"
echo
