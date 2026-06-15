# atefleet — scoped-context POC (kubectx-style owner scoping)

- **Date:** 2026-06-10
- **Status:** Design approved in brainstorming; pending spec review
- **Author:** Eliran Wolff

## Goal

A simple POC that gives the `atefleet` CLI a **kubeconfig-style config of named contexts**, where the active context asserts an **`owner`**, and the `FleetManager` **scopes** operations to that owner:

- `dispatch` / `run` create actors **stamped with the caller's owner**.
- `ls` returns **only the caller's** actors.
- `get` / `rm` act only on the caller's actors (others appear absent).

## Non-goals / explicitly out of scope (later)

- **Token verification.** This POC is **trusted-claim**: the context simply asserts an `owner` and the server trusts it. It is *cooperative scoping, not a security boundary* — any caller can assert any owner. Upgrading to verified tokens (a `token:` field + a server-side `token → owner` map) is a deliberate later step (see *Forward path*).
- Group scoping, per-context credentials/TLS, multi-tenancy enforcement.

## Background (what it builds on)

- `FleetManager` (`cmd/atefleet`) today has **no client auth**; `dialFleet` (`cli.go`) sends no identity. (Confirmed reference: Google's AX uses the same posture — insecure dial, Envoy-delegated authz.)
- Fleet metadata already carries `owner` (`FleetMeta`, stored in the valkey index); `DispatchActor`/`RunSubtask` accept an `owner`, and `ListFleet` already filters by `owner`. So scoping is mostly *forcing* those existing fields from an asserted identity.

## Design

### 1. Config file
Location: `~/.atefleet/config.yaml` (override with `$ATEFLEET_CONFIG`). Flat, kubeconfig-flavored:
```yaml
current-context: alice-kind
contexts:
  - {name: alice-kind, fleet-addr: localhost:18443, owner: alice}
  - {name: bob-prod,  fleet-addr: atefleet.ate-system.svc:443, owner: bob}
```
Serialized with the repo's existing YAML dependency (no new dep). A context is `{name, fleet-addr, owner}`. *Forward path:* a context gains an optional `token:` field when auth upgrades to verified tokens — no restructuring.

### 2. CLI — `ctx` command group (the kubectx part)
- `atefleet ctx` (alias `ctx list`) — list contexts; `*` marks `current-context`.
- `atefleet ctx use <name>` — set `current-context` (writes the file).
- `atefleet ctx set <name> --fleet-addr <addr> --owner <owner>` — add or update a context (writes the file).

**Resolution precedence** (nothing breaks for existing flag/env users): explicit `--fleet-addr` / `--owner` flag → `$ATEFLEET_FLEET_ADDR` / `$ATEFLEET_OWNER` env → active context from the config file → built-in default (`atefleet.ate-system.svc:443`, no owner). Every client subcommand (`dispatch`/`ls`/`get`/`rm`/`run`) dials the resolved `fleet-addr` and attaches `x-atefleet-owner: <owner>` gRPC metadata (omitted when no owner resolves).

### 3. Server — one interceptor + small per-handler guards
- A **unary interceptor** reads the `x-atefleet-owner` metadata into the request context; `ownerFromCtx(ctx) string` exposes it (empty = unscoped).
- A `--require-owner` flag (default **false** = current unscoped behavior; the demo turns it **true**). When true and no owner is present → `codes.Unauthenticated`.
- Enforcement when a scope owner is present:
  - `DispatchActor` / `RunSubtask`: set `meta.Owner = scopeOwner` (override any request-supplied owner).
  - `ListFleet`: force the owner filter to `scopeOwner` (regardless of the request's `owner`).
  - `GetFleetActor` / `TerminateActor`: if the target actor's `meta.Owner != scopeOwner` → `codes.NotFound` (do not reveal others' actors).

### 4. Components (small, isolated)
- `cmd/atefleet/internal/axconfig` — config load/save (YAML), `Active() (Context, error)`, `Use(name)`, `Set(Context)`, and a `Resolve(flagAddr, flagOwner) (addr, owner string)` applying the precedence. One file; testable without a cluster.
- `cmd/atefleet/ctx.go` — the three `ctx` subcommands (thin wrappers over `axconfig`).
- `cli.go` — `dialFleet` resolves via `axconfig` and attaches the owner metadata; client cmds unchanged otherwise.
- server: `ownerUnaryInterceptor` + `ownerFromCtx` (in `server.go` or a small `interceptors.go`); ~3-line guards in the five handlers; `--require-owner` wired in `serve.go`.

### 5. Data flow
CLI: load config → `Resolve` active `{addr, owner}` → dial `addr` → attach `x-atefleet-owner=owner` → RPC.
Server: interceptor extracts owner → handler stamps (create) / filters (list) / ownership-checks (get, rm).

### 6. Error handling
- Missing config file → built-in defaults, unscoped (no owner header).
- `current-context` naming a missing/empty context → clear CLI error.
- `--require-owner` + no owner metadata → `Unauthenticated`.
- `get`/`rm` on another owner's actor → `NotFound` (no existence leak).
- Malformed config YAML → CLI error naming the file + parse error.

### 7. Testing
- **axconfig** (no cluster): load/save round-trip on a tmp file; `Resolve` precedence (flag > env > context > default); `Use`/`Set` mutate + persist; missing/bad config behaviors.
- **server** (existing fake `ControlAPI` + miniredis): interceptor extracts owner from metadata; `DispatchActor` stamps `owner=scope` (overriding the request); `ListFleet` returns only scope's actors; `GetFleetActor`/`TerminateActor` on a cross-owner id → `NotFound`; `--require-owner` rejects a no-owner call with `Unauthenticated`.

## Forward path (not in this POC)
Verified tokens: add `token:` to a context; the CLI sends it as `authorization: Bearer <token>`; the server resolves `token → owner` from a config/Secret (and rejects unknown tokens) instead of trusting the asserted owner. The interceptor + handler enforcement stay the same — only the *source* of `scopeOwner` changes from "asserted" to "verified."
