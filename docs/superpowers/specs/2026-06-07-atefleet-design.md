# `atefleet` — actor fleet manager for agent-substrate

- **Date:** 2026-06-07
- **Status:** Draft (design approved in brainstorming; pending spec review)
- **Author:** Eliran Wolff
- **Name:** `atefleet` (placeholder, fits the `ate*` family; alt: `atemgr`)

## Goal

A binary that **dispatches and supervises a fleet of Actors** on an agent-substrate
cluster — so LLM/Claude-style agents run as substrate **Actors** (gVisor-sandboxed,
suspend-when-idle, resume-on-request), managed as a fleet, with the ability to fan out
one-shot sub-tasks.

`atefleet` is an in-cluster **gRPC service** (a Deployment in `ate-system`, like
`ateapi`) plus a bundled **CLI** and a **`run` command-wrapper**. It is the higher-level
manager that callers (a service, an orchestrator, a Claude Code hook, or a human) use to
spin up and manage agents on the cluster.

## Background — what it builds on

Substrate already provides the primitives; `atefleet` layers on top:

- **`ateapi`** — control-plane gRPC for per-Actor lifecycle: `CreateActor`, `ResumeActor`,
  `SuspendActor`, `DeleteActor`, `ListActors`, plus the `Actor`/`ActorTemplate` model.
- **`atenet`** — DNS + Envoy router: an HTTP request to
  `<actor-id>.actors.resources.substrate.ate.dev` **resumes the Actor if suspended** and
  forwards the request. This is the "turn" transport for long-lived agents.
- **`ActorTemplate`** — defines an Actor's workload image/command/env. This is how a user
  brings their own agent: any image that satisfies the contract.
- **Actor suspend/resume** — the substrate superpower (validated by the gVisor POC):
  in-memory state survives suspend; resume is fast. This is what makes a large fleet of
  mostly-idle agents cheap.

**The agent contract (pluggable / bring-your-own):** a long-lived agent is an Actor whose
workload is an HTTP server on `:80` that "handles one turn" per request. (`demos/counter`
is the canonical shape.) `atefleet` is agent-agnostic — it only needs the `ActorTemplate`.

## Key principles (locked in brainstorming)

- **Everything is an Actor.** "Agent" = Actor; "fleet" = the set of Actors `atefleet`
  manages. Terminology is uniformly *Actor*.
- **Actors are the source of truth.** `atefleet` keeps **no second registry** — the fleet
  view is derived from `ateapi.ListActors` filtered by labels `atefleet` stamps. The only
  `atefleet`-owned state is a small sub-task tracker (parent→child links + result
  rendezvous).
- **Not in the per-turn hot path.** Turns flow caller → `atenet` → Actor. `atefleet` only
  dispatches/manages, so it can be small and restart without dropping agent traffic.
- **Reuse, don't reinvent.** `DispatchActor` ≈ `CreateActor` + `ResumeActor` + stamp
  labels. Per-Actor CPU/mem is owned by the `ActorTemplate`/pod spec, not `atefleet`.

## Non-goals

- Not a replacement for `ateapi`/`atenet`/`kubectl-ate`; it sits above them.
- Not in the turn hot path; does not proxy or route agent traffic (that's `atenet`).
- Not a telemetry/token-saver tool — that's **Visor** (`ipp-safety-tools/.../visor`), which
  is complementary: Visor compacts tool *output* + records telemetry locally; `atefleet`
  dispatches/manages Actors on the cluster. They can coexist (the `run` wrapper below is a
  deliberate structural analog of `visor run`, but offloads to substrate rather than
  compacting).
- Per-Actor resource sizing (handled by `ActorTemplate`).

## Two Actor lifecycles (the "mix")

1. **Long-lived Actors** — persistent, stateful, **suspend-when-idle**, resumed on the next
   request (one HTTP turn via `atenet`). Created by `DispatchActor`. The fleet is mostly
   these: thousands of cheap dormant agents woken instantly.
2. **One-shot sub-task Actors** — run-to-completion jobs: `CreateActor → ResumeActor (runs
   it) → collect stdout/exit → DeleteActor`. **No `atenet` routing.** Surfaced as
   `atefleet run -- <cmd>` (transparent offload, `visor run`-style) and the `RunSubtask`
   RPC. A long-lived Actor fans out work by calling `RunSubtask` (it does **not** route
   anything itself).

## Architecture & data flows

```
caller (service / orchestrator / Claude hook / human / another Actor)
   │  gRPC (or `atefleet` CLI / `atefleet run`)
   ▼
atefleet  (Deployment in ate-system; gRPC FleetManager)
   │  ateapi gRPC: Create/Resume/Suspend/Delete/ListActors   (+ stamps fleet labels)
   ▼
ateapi ──> Actors (gVisor pods)         atenet ──> resumes Actor on inbound turn
```

- **Dispatch (long-lived):** caller → `atefleet.DispatchActor(template_ref, role, owner,
  group, idle_ttl)` → `ateapi.CreateActor` (+ first `ResumeActor`) → stamp labels
  (`ate.dev/fleet-role|owner|group`, `ate.dev/fleet-ttl`) → return `{actor_id, address}`
  (the `atenet` hostname). **`atefleet` is done; turns bypass it.**
- **Turn (hot path, `atefleet` uninvolved):** caller/Actor → HTTP to
  `<actor-id>.actors…` → `atenet` resumes if suspended → Actor handles the turn → responds.
- **Idle → suspend:** existing substrate auto-suspend; `atefleet` sets the per-Actor
  idle-TTL *policy* (carried as an Actor label/annotation consumed by the suspend path).
- **Sub-task / `run` (one-shot):** caller (or a running Actor) → `atefleet.RunSubtask(
  template_ref, input|cmd)` → `atefleet`: `CreateActor → ResumeActor` → collect result →
  `DeleteActor` → return `{stdout, exit}`. `atefleet run -- <cmd>` is the CLI front-end
  that streams real stdout/stderr and returns the real exit code.

## Components

| Component | Responsibility | Depends on |
|-----------|----------------|------------|
| `FleetManager` gRPC server | the dispatch/manage API | — |
| `ateapi` client | Create/Resume/Suspend/Delete/ListActors | `ateapi` |
| Fleet view | derive fleet from `ListActors` + label filter (no DB) | `ateapi` |
| Reaper | enforce TTLs; reap finished one-shot / orphaned Actors | `ateapi` |
| Sub-task tracker | small owned state: parent→child links + result rendezvous | (own store / annotations) |
| CLI + `atefleet run` | human/shim front-end; transparent one-shot offload | `FleetManager` |

## API surface (`FleetManager` gRPC)

**MVP (Phase 1) — (a) dispatch + (b) registry-view + thin (d) policy:**
- `DispatchActor(template_ref, role, owner, group, idle_ttl) → {actor_id, address}`
- `ListActors(filter{role,owner,group,status}) → [actor]` — derived view
- `GetActor(actor_id) → {state, address, labels}`
- `TerminateActor(actor_id)` — `DeleteActor`

**Phase 2 — (c) sub-task fan-out:**
- `RunSubtask(template_ref, input|cmd) → {stdout, exit}` (synchronous; `CreateActor →
  Resume → collect → Delete`)
- `atefleet run -- <cmd>` CLI wrapper over `RunSubtask`

**Phase 3 — (d-full) policy + (e) observability:**
- `SetActorPolicy(actor_id, idle_ttl, …)`; fleet **count-quotas** (max Actors per
  pool/owner/group)
- `WatchFleet(filter) → stream`; `GetActorLogs(actor_id)` — reuse `kubectl-ate logs` /
  `ateapi` where possible; `atefleet` adds the fleet-grouped lens
- Optional Claude Code **hook trigger**: a `PreToolUse`/`SubagentStart` hook (command or
  `http`) that calls `atefleet` to dispatch/`run` (observe/trigger only — see note)

> **Claude-hook note (decided in brainstorming):** a hook *cannot* transparently swap a
> local command's output for a remote result. The transparent path is the **`atefleet run`
> wrapper** (it *is* the command, like `visor run`); a hook can only *trigger/observe* it.

## Fleet state model

- **Source of truth = Actors.** `atefleet` stamps labels on each Actor it creates:
  `ate.dev/fleet-role`, `ate.dev/fleet-owner`, `ate.dev/fleet-group`, `ate.dev/fleet-ttl`,
  and for sub-tasks `ate.dev/fleet-parent`. The fleet view is `ListActors` + label filter.
- **`atefleet`-owned state (minimal):** sub-task parent→child links + result rendezvous
  (for `RunSubtask`/`run` to await + return results). Start with **Actor annotations**;
  promote to a small store (reuse substrate's Redis/valkey) only if needed.

## Lifecycle policy (d)

- **Idle-suspend TTL** per Actor (policy label consumed by the suspend path).
- **Count-quotas:** max Actors per pool / owner / group (fleet-level guard against
  runaway dispatch). Per-Actor CPU/mem stays in the `ActorTemplate`.
- **Reaper:** background loop — delete Actors past their `fleet-ttl`; clean up one-shot
  Actors whose `RunSubtask` finished (or leaked, as a backstop).

## Error handling

- `RunSubtask`/`run` **always** tear the Actor down on completion *or* failure (no leaks);
  the `fleet-ttl` reaper is the backstop if `atefleet` crashes mid-run.
- `ateapi` errors are surfaced to the caller with context (dispatch failed at
  Create/Resume, etc.). Idempotent retries where safe (e.g. dispatch is create-or-get).
- Reaper is idempotent (delete-if-present); safe to run on every `atefleet` replica/restart
  (guard with a lease if multi-replica).

## Testing

- **Unit** (against a **fake `ateapi` client**): label-filter fleet view, reaper TTL/orphan
  logic, `RunSubtask` create→collect→delete + cleanup-on-error, `run`-wrapper arg/stdout/
  exit handling.
- **Integration:** `FleetManager` against a real/fake `ateapi`; reuse substrate's existing
  test patterns (miniredis, controller-runtime fakes).
- **e2e (later):** dispatch a long-lived Actor (`demos/counter`-style) + a `RunSubtask`
  one-shot, on a gVisor-prepared cluster — building on the gVisor POC's environment.

## Phasing (each phase = its own spec + plan)

1. **MVP — fleet service + dispatch.** gRPC `DispatchActor`/`ListActors`/`GetActor`/
   `TerminateActor`, fleet labels, TTL reaper, the `atefleet` CLI. Long-lived Actors only.
   *(a)+(b)+thin (d).*
2. **Sub-tasks.** `RunSubtask` + `atefleet run` wrapper (transparent one-shot offload).
   *(c).*
3. **Policy + observability + hook trigger.** count-quotas, `WatchFleet`/`GetActorLogs`
   lens, optional Claude `PreToolUse`/`SubagentStart` → `atefleet` trigger. *(d-full)+(e).*

## Open questions / future

- **Result transport for `RunSubtask`:** MVP uses **run-to-completion stdout/exit** (the
  `run`-wrapper case); long-lived agent-style sub-tasks could instead take one HTTP turn —
  support both behind a `mode` once Phase 2 lands.
- **`atefleet` deployment auth:** gRPC mTLS reusing substrate's pod-cert pattern (as
  `ateapi`/`atenet` do).
- **Multi-replica `atefleet`:** reaper leasing / leader election if >1 replica.
- **Relationship to `kubectl-ate`:** `atefleet`'s CLI may eventually subsume or complement
  the actor verbs in `kubectl-ate`; keep them consistent.
