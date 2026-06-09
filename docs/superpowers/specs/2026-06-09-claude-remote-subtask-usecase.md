# Use case: Claude Code dispatches short tasks to remote substrate actors

- **Date:** 2026-06-09
- **Status:** Use-case definition (to align before building/testing)
- **Relates to:** `atefleet` design (`docs/superpowers/specs/2026-06-07-atefleet-design.md`) — this is the **Phase 2 (`RunSubtask`/`atefleet run`) + Phase 3 (Claude hook)** slice, *not* the Phase 1 long-lived fleet that is already built and live-tested.

## One-liner

While a developer works in Claude Code, short self-contained tasks are **automatically offloaded** to **ephemeral, gVisor-sandboxed actors** on an agent-substrate cluster. Each actor runs the task to completion, returns the result, and is torn down — so heavy / parallel / untrusted work runs remotely and safely instead of on the laptop.

## Who/what is in the story

- **Developer + Claude Code** — local, the originator of tasks.
- **`atefleet`** — the dispatcher on the cluster (the binary we built).
- **A one-shot "task-runner" `ActorTemplate`** — a gVisor sandbox image that executes a given task (command and/or agent prompt) and emits a result. (Candidate workloads: `demos/sandbox`, or `demos/claude-code-multiplex`, or a purpose-built runner.)
- **substrate execution** — `ateapi` → `atelet` → `ateom-gvisor` → `runsc` actually runs the sandbox.

## The flow (one short task)

1. Claude Code is about to run a short, self-contained task — e.g. run the test suite, a lint, a codemod, a build, or a focused sub-agent prompt.
2. **Instead of running it locally**, the task is dispatched to the cluster, via one of the two mechanisms below.
3. `atefleet.RunSubtask(template, input|cmd)`:
   `CreateActor` → `ResumeActor` (boots a fresh gVisor sandbox) → actor executes the task → atefleet **collects stdout/stderr/exit** → `DeleteActor` (teardown). One-shot: nothing lingers.
4. The result is returned to Claude Code **as if the command had run locally** (stdout + real exit code).

## Why

- Move heavy / long / parallel work **off the laptop** (and fan many out at once).
- Run **model-generated / untrusted code** under strong **gVisor** isolation, not on the dev machine.
- Elastic remote capacity; ephemeral so there is no idle cost.

## Two trigger mechanisms (the key design point)

- **(A) Wrapper — transparent.** `atefleet run -- <cmd>` *is* the command: it offloads, streams real stdout/stderr, and returns the real exit code (structurally like `visor run`). This is the **only** way to transparently substitute a remote result for a local one. "Automatic" here = Claude is configured/instructed to use the wrapper for offloadable tasks.
- **(B) Claude Code hook — trigger/observe only.** A `PreToolUse` / `SubagentStart` hook (command or `http` type) fires when Claude is about to spawn a sub-agent or run a matching command, and calls atefleet to dispatch. **Constraint (decided earlier):** a hook **cannot** transparently feed the remote result back in place of the local tool's output — it can only *trigger/observe* (at most block + annotate). So the result path still needs the wrapper.

## Gap vs. what exists today

- **Built + live-tested (Phase 1):** long-lived fleet — `DispatchActor` / `ListFleet` / `GetFleetActor` / `TerminateActor` + TTL reaper. **This is *not* this use case.**
- **Needed for this use case:**
  - `RunSubtask` RPC + `atefleet run` CLI wrapper (**Phase 2 — not built**).
  - A one-shot task-runner `ActorTemplate` (sandbox that runs a given command/prompt and returns output).
  - Optionally the Claude Code hook (**Phase 3**) to make it "automatic."

## A minimal live test of THIS use case

1. Implement `RunSubtask` + `atefleet run`.
2. Define a task-runner `ActorTemplate` (a sandbox that runs `$@` and returns stdout/exit).
3. `atefleet run -- <cmd>` → observe the task execute **inside a remote gVisor actor**, return stdout + exit, and the actor be created → resumed → deleted (one-shot).
4. (Optional) wire a Claude Code `PreToolUse`/`SubagentStart` hook to auto-trigger it.

## Open questions (to align before building)

- **What is a "short task"?** A shell command (`Bash`), a Claude sub-agent prompt (`Task` tool), or both?
- **Sync or async?** Block for the result (`run`-style) vs. fire-and-forget + collect later.
- **Actor workload:** a generic command-runner sandbox, or a full agent runtime (`claude-code-multiplex`)?
- **"Automatic" =** Claude defaulting to the wrapper, or a hook intercepting? (Confirm the hook-can't-substitute-output constraint is acceptable.)
- **Result transport:** run-to-completion stdout/exit, vs. one HTTP turn to the actor.
