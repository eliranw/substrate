# `atefleet` Phase 2 — one-shot sub-tasks (`RunSubtask` + `atefleet run`)

- **Date:** 2026-06-09
- **Goal:** A caller (or a Claude Code hook/wrapper) dispatches a **short, self-contained command** to an **ephemeral gVisor actor**, which runs it to completion and returns `{stdout, stderr, exit}`; the actor is then torn down. Synchronous, run-to-completion, command-based.
- **Relates to:** `docs/superpowers/specs/2026-06-09-claude-remote-subtask-usecase.md` + the atefleet design (Phase 2).

## Key decisions (grounded in scouting the live repo)

- **Reuse `demos/sandbox`** as the task-runner workload — it already is an HTTP server: `POST /process` with `{"command":[...],"envvars":{},"cwd":"","timeout":"30s"}` → `{"stdout","stderr","exitCode","error"}` (see `demos/sandbox/main.go`). No new workload needed.
- **Transport:** after Resume, atefleet HTTP-POSTs directly to the actor's `AteomPodIp:80/process` (from `ateapi.GetActor`). In-cluster pod-to-pod; no atenet dependency for the sync case.
- **Teardown:** ateapi `DeleteActor` only deletes a **suspended** actor (proven live), so RunSubtask does `SuspendActor` → `DeleteActor`. A short-TTL index entry is the reaper backstop against leaks if atefleet crashes mid-run.
- **Out of scope:** the Claude-*prompt* variant (vs shell command) and async/fire-and-forget — follow-ups.

## Scouted facts the implementer can rely on

- sandbox contract: `POST /process`, req `{command []string, envvars map, cwd string, timeout string}`, resp `{stdout string, stderr string, exitCode int, error string}`.
- `ateapipb.SuspendActorRequest{ActorId}` / `SuspendActorResponse{Actor}` exist.
- `ateapi.GetActor(...).GetActor().GetAteomPodIp()` returns the worker pod IP once RUNNING.
- Existing `NewServer(api ControlAPI, idx *Index, now func() int64)` — Phase 2 adds a `SubtaskRunner` dependency.
- sandbox template (from `demos/sandbox/sandbox.yaml.tmpl`): ns `ate-demo-sandbox`, ActorTemplate `sandbox-template`, WorkerPool `sandbox-workerpool`.

## Tasks

### T1 — `RunSubtask` proto
Add to `internal/proto/atefleetpb/atefleet.proto`:
```proto
rpc RunSubtask(RunSubtaskRequest) returns (RunSubtaskResponse) {}

message RunSubtaskRequest {
  string actor_template_namespace = 1;
  string actor_template_name      = 2;
  repeated string command         = 3;
  int64  timeout_seconds          = 4; // 0 = no explicit timeout
  string owner                    = 5;
  string group                    = 6;
}
message RunSubtaskResponse {
  string stdout    = 1;
  string stderr    = 2;
  int32  exit_code = 3;
  string error     = 4; // non-empty if the actor reported a run error
  string actor_id  = 5; // the ephemeral actor that ran it
}
```
`go generate ./...` in the proto dir; build.

### T2 — `SuspendActor` on the `ControlAPI` seam
Add `SuspendActor(ctx, *ateapipb.SuspendActorRequest) (*ateapipb.SuspendActorResponse, error)` to the `ControlAPI` interface + `controlAdapter`. Add it to the test `fakeControl` (record `suspended []string`, flip status → SUSPENDED).

### T3 — actor HTTP runner seam (TDD)
`cmd/atefleet/internal/fleet/runner.go`: an interface
```go
type SubtaskRunner interface {
    Run(ctx context.Context, addr string, command []string, timeout time.Duration) (stdout, stderr string, exitCode int, runErr string, err error)
}
```
Real impl `httpRunner` POSTs `{command,timeout}` JSON to `http://<addr>/process`, decodes the sandbox response. Small bounded retry (the workload may need a beat to serve after resume). `addr` = `<AteomPodIp>:80`. Tests fake the interface.

### T4 — `RunSubtask` handler (TDD)
`Server.RunSubtask`:
1. Validate template ns/name + non-empty command.
2. Generate a unique DNS-1123 actor id (e.g. `subtask-<uuid>`).
3. Index a `FleetMeta{Role:"subtask", Owner, Group, ExpiryUnix: now()+backstopTTL}` (reaper backstop).
4. `CreateActor` + `ResumeActor`.
5. `GetActor` → `AteomPodIp`; `runner.Run(addr, command, timeout)`.
6. **`defer` teardown**: `SuspendActor` then `DeleteActor` + index `Delete` — runs even if the POST fails, so no actor leaks.
7. Return `{stdout, stderr, exit_code, error, actor_id}`.
Thread a `SubtaskRunner` into `NewServer` (update `serve.go` + `newTestServer`). Tests: success (stdout/exit returned + create/resume/suspend/delete all called + index cleaned), and run-error still tears down.

### T5 — `atefleet run` CLI + serve wiring
`newRunCmd`: `atefleet run --template <ns>/<name> [--timeout 30s] [--owner --group] -- <cmd...>`. Calls `RunSubtask`, writes stdout→os.Stdout, stderr→os.Stderr, `os.Exit(exit_code)`. Wire an `httpRunner` into `serve.go`'s `NewServer`. Register `newRunCmd` in `main.go`.

### T6 — live e2e (GATE, manual)
Deploy `demos/sandbox`; rebuild+redeploy atefleet; `atefleet run --template ate-demo-sandbox/sandbox-template -- echo hello` → stdout `hello`, exit 0; confirm the ephemeral actor was created → resumed → deleted.

## Definition of done
- `RunSubtask` + `atefleet run` build, unit-tested (fake ControlAPI + fake runner + miniredis).
- Live: a shell command runs inside a remote gVisor actor and returns stdout/exit; the actor is one-shot (gone after).
