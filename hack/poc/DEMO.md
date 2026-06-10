# atefleet — manual demo (click commands one-by-one)

A live walkthrough of the **atefleet** FleetManager against a real substrate cluster.
Open the **dashboard** in one window and click these commands one-by-one in a terminal;
the dashboard updates live from the **valkey index**.

---

## Setup (once)

```bash
cd /Users/eliranw/conductor/workspaces/substrate/miami

# 1. build the binaries
go build -o bin/atefleet ./cmd/atefleet
go build -o bin/kubectl-ate ./cmd/kubectl-ate

# 2. reach the in-cluster FleetManager from your laptop (kind = ClusterIP)
KUBECONFIG=~/.kube/atefleet-kind.config kubectl port-forward -n ate-system svc/atefleet 18443:443
#    ^ leave this running in its own terminal
```

```bash
# 3. start the LIVE DASHBOARD (own terminal) — serves it + feeds it from the valkey index every 1s
cd /Users/eliranw/conductor/workspaces/substrate/miami
hack/poc/atefleet-dashboard.sh        # opens http://localhost:8899/
```

```bash
# 4. in your "demo" terminal, a short alias so the commands read cleanly
cd /Users/eliranw/conductor/workspaces/substrate/miami
alias af='bin/atefleet --fleet-addr localhost:18443'
```

> Watch the dashboard as you run each step below. **Metadata (role/owner/group/ttl)
> is shown straight from atefleet's valkey index; status comes from ateapi.**

---

## The demo

**1 — Dispatch a fleet of long-lived actors (each a gVisor-sandboxed, suspend-when-idle agent).**
atefleet stamps `role/owner/group/ttl` into its valkey index, and returns the actor's address.
```bash
af dispatch --id repo-acme    --template ate-demo-counter/counter --role reviewer --owner platform --group prod    --ttl 1h
```
```bash
af dispatch --id repo-globex  --template ate-demo-counter/counter --role reviewer --owner platform --group prod    --ttl 1h
```
```bash
af dispatch --id repo-initech --template ate-demo-counter/counter --role scanner  --owner security --group staging --ttl 30m
```
*Watch:* three rows appear in **Fleet**, and three `atefleet:actor:<id>` records appear in **valkey index**.

**2 — The fleet view, and filtering by the metadata in the index.**
```bash
af ls
```
```bash
af ls --group prod
```
```bash
af ls --owner security
```

**3 — Inspect one actor as raw index data (status ⊕ metadata).**
```bash
af get repo-acme -o json
```

**4 — One-shot sub-tasks: run a command inside an ephemeral remote gVisor actor.**
This is the Claude-Code use case — dispatch a short command, get stdout/exit, actor torn down.
Use `arun` so the run also shows in the dashboard's sub-task panel:
```bash
hack/poc/arun -- echo "hello from a remote, sandboxed actor"
```
```bash
hack/poc/arun -- sh -c 'uname -a; echo "running as uid $(id -u)"'      # note: runsc / -gvisor kernel
```
```bash
hack/poc/arun -- sh -c 'echo to-stdout; echo to-stderr 1>&2; exit 7'   # exit code + stderr propagate
```
```bash
af ls            # still just the 3 fleet actors — sub-tasks left nothing behind (one-shot)
```
*Watch:* entries fill the **sub-task runs** panel (with a `gVisor ✓` badge), while **Fleet** is unchanged.

**5 — (optional) Prove the metadata really lives in valkey — read it raw from the cluster.**
```bash
KUBECONFIG=~/.kube/atefleet-kind.config kubectl exec -n ate-system valkey-cluster-0 -c valkey -- \
  valkey-cli --tls --cacert /etc/valkey-ca/ca.crt \
  --cert /run/servicedns.podcert.ate.dev/credential-bundle.pem \
  --key  /run/servicedns.podcert.ate.dev/credential-bundle.pem \
  -p 6379 -c SMEMBERS atefleet:actor-ids
```
*(If the cert flags differ on your build, the dashboard's "valkey index" panel already shows the
same records via atefleet's own read path — `SMEMBERS atefleet:actor-ids` → `GET atefleet:actor:<id>`.)*

**6 — Terminate a fleet actor (suspend, then remove — ateapi only deletes suspended actors).**
```bash
bin/kubectl-ate --kubeconfig ~/.kube/atefleet-kind.config suspend actor repo-initech
```
```bash
af rm repo-initech
```
*Watch:* `repo-initech` drops out of **Fleet** and its `atefleet:actor:` record disappears from **valkey index**.

---

## Reset
```bash
for id in repo-acme repo-globex repo-initech; do
  bin/kubectl-ate --kubeconfig ~/.kube/atefleet-kind.config suspend actor "$id" 2>/dev/null
  af rm "$id" 2>/dev/null
done
```
