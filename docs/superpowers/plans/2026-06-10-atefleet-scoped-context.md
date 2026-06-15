# atefleet scoped-context POC — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the `atefleet` CLI a kubeconfig-style multi-context config whose active context asserts an `owner`, and have the `FleetManager` scope `dispatch`/`run`/`ls`/`get`/`rm` to that owner (trusted-claim — cooperative scoping, not a security boundary).

**Architecture:** The CLI resolves `{fleet-addr, owner}` (flag > env > active context > default) from `~/.atefleet/config.yaml` and attaches an `x-atefleet-owner` gRPC metadata header on every call. The server reads it (`ownerFromCtx`), an opt-in `--require-owner` interceptor gates it, and the handlers stamp it on create, force-filter it on list, and ownership-check it on get/rm. No proto change.

**Tech Stack:** Go 1.26, gRPC + `google.golang.org/grpc/metadata` (existing dep), `sigs.k8s.io/yaml` v1.6.0 (existing dep; uses `json:` struct tags), cobra, the existing fake-`ControlAPI` + miniredis test harness.

**Spec:** `docs/superpowers/specs/2026-06-10-atefleet-scoped-context-design.md`.

---

## File structure

- `cmd/atefleet/internal/axconfig/axconfig.go` — **Create.** Config load/save + `Active`/`Use`/`Set`/`Resolve`.
- `cmd/atefleet/internal/axconfig/axconfig_test.go` — **Create.**
- `cmd/atefleet/internal/fleet/owner.go` — **Create.** `OwnerMetadataKey`, `ownerFromCtx`, `OwnerUnaryInterceptor`.
- `cmd/atefleet/internal/fleet/owner_test.go` — **Create.**
- `cmd/atefleet/internal/fleet/server.go` — **Modify.** Scope-enforce in `DispatchActor`/`RunSubtask`/`ListFleet`/`GetFleetActor`/`TerminateActor`.
- `cmd/atefleet/internal/fleet/server_test.go` — **Modify.** Add scoping tests.
- `cmd/atefleet/serve.go` — **Modify.** `--require-owner` flag + chain the interceptor.
- `cmd/atefleet/cli.go` — **Modify.** `dialFleet` resolves via `axconfig` + appends owner metadata; `--fleet-addr` default → `""`; add persistent `--owner`.
- `cmd/atefleet/ctx.go` — **Create.** `ctx list|use|set` subcommands.
- `cmd/atefleet/main.go` — **Modify.** Register `newCtxCmd()`.

---

## Task 1: `axconfig` package (TDD)

**Files:** Create `cmd/atefleet/internal/axconfig/axconfig.go`, `axconfig_test.go`.

- [ ] **Step 1: Write the failing test** — `axconfig_test.go`:

```go
package axconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePrecedence(t *testing.T) {
	cfg := &Config{CurrentContext: "a", Contexts: []Context{{Name: "a", FleetAddr: "ctx-addr", Owner: "ctx-owner"}}}

	// flag wins
	addr, owner := cfg.Resolve("flag-addr", "flag-owner")
	if addr != "flag-addr" || owner != "flag-owner" {
		t.Fatalf("flag precedence: got %q %q", addr, owner)
	}
	// env beats context
	t.Setenv("ATEFLEET_FLEET_ADDR", "env-addr")
	t.Setenv("ATEFLEET_OWNER", "env-owner")
	addr, owner = cfg.Resolve("", "")
	if addr != "env-addr" || owner != "env-owner" {
		t.Fatalf("env precedence: got %q %q", addr, owner)
	}
	// context beats default when no flag/env
	os.Unsetenv("ATEFLEET_FLEET_ADDR")
	os.Unsetenv("ATEFLEET_OWNER")
	addr, owner = cfg.Resolve("", "")
	if addr != "ctx-addr" || owner != "ctx-owner" {
		t.Fatalf("ctx precedence: got %q %q", addr, owner)
	}
	// default when nothing set
	empty := &Config{}
	addr, owner = empty.Resolve("", "")
	if addr != DefaultFleetAddr || owner != "" {
		t.Fatalf("default: got %q %q", addr, owner)
	}
}

func TestLoadSaveUseSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("ATEFLEET_CONFIG", path)

	// missing file -> empty config, no error
	c, err := Load()
	if err != nil || len(c.Contexts) != 0 {
		t.Fatalf("load missing: %+v %v", c, err)
	}
	// set adds a context and (first one) becomes current; persists
	if err := c.Set(Context{Name: "alice", FleetAddr: "localhost:18443", Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	c2, _ := Load()
	if c2.CurrentContext != "alice" || len(c2.Contexts) != 1 || c2.Contexts[0].Owner != "alice" {
		t.Fatalf("after set: %+v", c2)
	}
	// use unknown -> error
	if err := c2.Use("nope"); err == nil {
		t.Fatal("expected error using unknown context")
	}
	// Active resolves the current context
	a, err := c2.Active()
	if err != nil || a.Owner != "alice" {
		t.Fatalf("active: %+v %v", a, err)
	}
}
```

- [ ] **Step 2: Run → FAIL.** `go test ./cmd/atefleet/internal/axconfig/ -v` (undefined Config/Load/...).

- [ ] **Step 3: Implement** `axconfig.go`:

```go
// Copyright 2026 Google LLC
// (Apache 2.0 header — match sibling files)

// Package axconfig is atefleet's kubeconfig-style context config: named
// contexts each asserting a fleet-addr and owner, with flag>env>context>default
// resolution.
package axconfig

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// DefaultFleetAddr is used when no flag/env/context supplies one.
const DefaultFleetAddr = "atefleet.ate-system.svc:443"

type Context struct {
	Name      string `json:"name"`
	FleetAddr string `json:"fleet-addr"`
	Owner     string `json:"owner,omitempty"`
}

type Config struct {
	CurrentContext string    `json:"current-context"`
	Contexts       []Context `json:"contexts"`
}

// path is $ATEFLEET_CONFIG or ~/.atefleet/config.yaml.
func path() string {
	if p := os.Getenv("ATEFLEET_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".atefleet", "config.yaml")
}

// Load reads the config; a missing file yields an empty config (not an error).
func Load() (*Config, error) {
	b, err := os.ReadFile(path())
	if os.IsNotExist(err) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path(), err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path(), err)
	}
	return &c, nil
}

func (c *Config) save() error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path(), b, 0o600)
}

// Active returns the current-context, or an error if unset/unknown.
func (c *Config) Active() (Context, error) {
	if c.CurrentContext == "" {
		return Context{}, fmt.Errorf("no current-context set")
	}
	for _, x := range c.Contexts {
		if x.Name == c.CurrentContext {
			return x, nil
		}
	}
	return Context{}, fmt.Errorf("current-context %q not found", c.CurrentContext)
}

// Use sets the current-context (must exist) and persists.
func (c *Config) Use(name string) error {
	for _, x := range c.Contexts {
		if x.Name == name {
			c.CurrentContext = name
			return c.save()
		}
	}
	return fmt.Errorf("context %q not found", name)
}

// Set upserts a context (by name) and persists. The first context added also
// becomes current-context for convenience.
func (c *Config) Set(ctx Context) error {
	for i, x := range c.Contexts {
		if x.Name == ctx.Name {
			c.Contexts[i] = ctx
			return c.save()
		}
	}
	c.Contexts = append(c.Contexts, ctx)
	if c.CurrentContext == "" {
		c.CurrentContext = ctx.Name
	}
	return c.save()
}

// Resolve applies precedence: flag > env > active context > default.
func (c *Config) Resolve(flagAddr, flagOwner string) (addr, owner string) {
	var act Context
	if a, err := c.Active(); err == nil {
		act = a
	}
	addr = first(flagAddr, os.Getenv("ATEFLEET_FLEET_ADDR"), act.FleetAddr, DefaultFleetAddr)
	owner = first(flagOwner, os.Getenv("ATEFLEET_OWNER"), act.Owner)
	return
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
```

- [ ] **Step 4: Run → PASS.** `go test ./cmd/atefleet/internal/axconfig/ -v`.
- [ ] **Step 5: Commit** — `git commit -am "atefleet: axconfig kubectx-style context config (TDD)"`.

---

## Task 2: owner metadata interceptor + `ownerFromCtx` (TDD)

**Files:** Create `cmd/atefleet/internal/fleet/owner.go`, `owner_test.go`.

- [ ] **Step 1: Write the failing test** — `owner_test.go`:

```go
package fleet

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestOwnerFromCtx(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(OwnerMetadataKey, "alice"))
	if got := ownerFromCtx(ctx); got != "alice" {
		t.Fatalf("got %q", got)
	}
	if got := ownerFromCtx(context.Background()); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestOwnerInterceptorRequire(t *testing.T) {
	h := func(ctx context.Context, req any) (any, error) { return "ok", nil }
	info := &grpc.UnaryServerInfo{}

	// require + no owner -> Unauthenticated
	_, err := OwnerUnaryInterceptor(true)(context.Background(), nil, info, h)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
	// require + owner present -> passes
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(OwnerMetadataKey, "alice"))
	if _, err := OwnerUnaryInterceptor(true)(ctx, nil, info, h); err != nil {
		t.Fatalf("want pass, got %v", err)
	}
	// not required + no owner -> passes
	if _, err := OwnerUnaryInterceptor(false)(context.Background(), nil, info, h); err != nil {
		t.Fatalf("want pass, got %v", err)
	}
}
```

- [ ] **Step 2: Run → FAIL.** `go test ./cmd/atefleet/internal/fleet/ -run TestOwner -v`.

- [ ] **Step 3: Implement** `owner.go`:

```go
// Copyright 2026 Google LLC  (Apache header)
package fleet

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// OwnerMetadataKey is the gRPC metadata header carrying the caller's asserted
// owner (trusted-claim scoping). Shared with the CLI client.
const OwnerMetadataKey = "x-atefleet-owner"

// ownerFromCtx returns the asserted owner from incoming gRPC metadata ("" = none).
func ownerFromCtx(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	if v := md.Get(OwnerMetadataKey); len(v) > 0 {
		return v[0]
	}
	return ""
}

// OwnerUnaryInterceptor gates calls when requireOwner is set: a call with no
// asserted owner is rejected. Otherwise it is a no-op (handlers read the owner
// via ownerFromCtx).
func OwnerUnaryInterceptor(requireOwner bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if requireOwner && ownerFromCtx(ctx) == "" {
			return nil, status.Errorf(codes.Unauthenticated, "missing %s metadata", OwnerMetadataKey)
		}
		return handler(ctx, req)
	}
}
```

- [ ] **Step 4: Run → PASS.** `go test ./cmd/atefleet/internal/fleet/ -run TestOwner -v`.
- [ ] **Step 5: Commit** — `git commit -am "atefleet: owner gRPC metadata interceptor (TDD)"`.

---

## Task 3: enforce owner scope in handlers (TDD)

**Files:** Modify `cmd/atefleet/internal/fleet/server.go`, `server_test.go`.

- [ ] **Step 1: Write the failing test** — append to `server_test.go`:

```go
func scoped(owner string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(OwnerMetadataKey, owner))
}

func TestScope_DispatchStampsOwnerAndListFilters(t *testing.T) {
	s, _ := newTestServer(t)
	// alice dispatches (request owner "bogus" must be overridden to "alice")
	if _, err := s.DispatchActor(scoped("alice"), &atefleetpb.DispatchActorRequest{
		ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: "a1", Owner: "bogus",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DispatchActor(scoped("bob"), &atefleetpb.DispatchActorRequest{
		ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: "b1",
	}); err != nil {
		t.Fatal(err)
	}
	// alice's index entry is stamped owner=alice
	m, _ := s.idx.Get(context.Background(), "a1")
	if m.Owner != "alice" {
		t.Fatalf("want owner alice, got %q", m.Owner)
	}
	// alice's ls sees only a1
	resp, err := s.ListFleet(scoped("alice"), &atefleetpb.ListFleetRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetActors()) != 1 || resp.GetActors()[0].GetActorId() != "a1" {
		t.Fatalf("alice ls = %+v", resp.GetActors())
	}
}

func TestScope_GetAndRmCrossOwnerNotFound(t *testing.T) {
	s, fc := newTestServer(t)
	s.DispatchActor(scoped("alice"), &atefleetpb.DispatchActorRequest{ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: "a1"})
	// bob cannot get alice's actor
	if _, err := s.GetFleetActor(scoped("bob"), &atefleetpb.GetFleetActorRequest{ActorId: "a1"}); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
	// bob cannot rm alice's actor (and DeleteActor must NOT be called)
	if _, err := s.TerminateActor(scoped("bob"), &atefleetpb.TerminateActorRequest{ActorId: "a1"}); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
	if len(fc.deleted) != 0 {
		t.Fatalf("DeleteActor should not be called cross-owner: %v", fc.deleted)
	}
}
```

(Ensure `server_test.go` imports `"google.golang.org/grpc/metadata"`, `"google.golang.org/grpc/codes"`, `"google.golang.org/grpc/status"` — add any missing.)

- [ ] **Step 2: Run → FAIL.** `go test ./cmd/atefleet/internal/fleet/ -run TestScope -v` (alice ls returns both; cross-owner get/rm succeed).

- [ ] **Step 3: Implement** — edit `server.go`:

In `DispatchActor`, before building `meta` (currently `server.go:81` uses `Owner: r.GetOwner()`), compute the effective owner:
```go
	owner := r.GetOwner()
	if scope := ownerFromCtx(ctx); scope != "" {
		owner = scope
	}
	// ...then in the FleetMeta literal use Owner: owner (instead of r.GetOwner())
	meta := FleetMeta{
		ActorID: r.GetActorId(), Role: r.GetRole(), Owner: owner, Group: r.GetGroup(),
		ExpiryUnix: expiry, TemplateNamespace: r.GetActorTemplateNamespace(), TemplateName: r.GetActorTemplateName(),
	}
```

In `RunSubtask`, same change where it builds the backstop `FleetMeta` (currently `server.go:178` `Owner: r.GetOwner()`):
```go
	owner := r.GetOwner()
	if scope := ownerFromCtx(ctx); scope != "" {
		owner = scope
	}
	// meta := FleetMeta{ ActorID: id, Role: "subtask", Owner: owner, Group: r.GetGroup(), ... }
```

In `ListFleet`, force the owner filter to the scope. Replace the owner-filter line (`server.go:110`):
```go
	wantOwner := r.GetOwner()
	if scope := ownerFromCtx(ctx); scope != "" {
		wantOwner = scope
	}
	// ...inside the loop, replace `if r.GetOwner() != "" && m.Owner != r.GetOwner()` with:
	if wantOwner != "" && m.Owner != wantOwner {
		continue
	}
```

In `GetFleetActor`, after loading the index meta `m` (it already does `idx.Get`), add the ownership check before returning success:
```go
	if scope := ownerFromCtx(ctx); scope != "" && m.Owner != scope {
		return nil, status.Error(codes.NotFound, "actor not in fleet")
	}
```

In `TerminateActor`, add an ownership check **before** calling `DeleteActor` (it currently deletes without reading the index). Insert at the top of the handler:
```go
	if scope := ownerFromCtx(ctx); scope != "" {
		m, err := s.idx.Get(ctx, r.GetActorId())
		if err == ErrNotFound || (err == nil && m.Owner != scope) {
			return nil, status.Error(codes.NotFound, "actor not in fleet")
		}
		if err != nil && err != ErrNotFound {
			return nil, status.Errorf(codes.Internal, "get index: %v", err)
		}
	}
```

- [ ] **Step 4: Run → PASS.** `go test -count=1 ./cmd/atefleet/internal/fleet/... -v` (all existing + new scope tests pass).
- [ ] **Step 5: Commit** — `git commit -am "atefleet: enforce owner scope in handlers (TDD)"`.

---

## Task 4: wire interceptor + `--require-owner` in `serve.go`

**Files:** Modify `cmd/atefleet/serve.go`.

- [ ] **Step 1: Add the flag** near the other `cmd.Flags()` calls (after `serve.go:134`):
```go
	var requireOwner bool
	cmd.Flags().BoolVar(&requireOwner, "require-owner", false, "Reject calls that do not assert an x-atefleet-owner (scoped mode).")
```
(Declare `requireOwner` with the other `var` declarations at the top of `newServeCmd` so it's in scope in `RunE`.)

- [ ] **Step 2: Chain the interceptor** — change the gRPC server construction (`serve.go:118-121`) to add the unary interceptor alongside the existing stats handler:
```go
	g := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(fleet.OwnerUnaryInterceptor(requireOwner)),
	)
```

- [ ] **Step 3: Build + vet.** Run: `go build ./cmd/atefleet/... && go vet ./cmd/atefleet/...`. Expected: clean.
- [ ] **Step 4: Commit** — `git commit -am "atefleet: --require-owner flag + chain owner interceptor"`.

---

## Task 5: CLI — resolve via axconfig + attach owner metadata

**Files:** Modify `cmd/atefleet/cli.go`.

- [ ] **Step 1: Add imports** to `cli.go`: `"context"`, `"google.golang.org/grpc/metadata"`, and `"github.com/agent-substrate/substrate/cmd/atefleet/internal/axconfig"`, `"github.com/agent-substrate/substrate/cmd/atefleet/internal/fleet"`.

- [ ] **Step 2: Add an `--owner` persistent flag and empty the `--fleet-addr` default** so `Resolve`'s precedence works. In `newRootCmd`, change the fleet-addr default to `""` and add:
```go
	cmd.PersistentFlags().StringVar(&fleetAddr, "fleet-addr", "", "FleetManager address (overrides env/context).")
	cmd.PersistentFlags().StringVar(&ownerFlag, "owner", "", "Owner to assert (overrides env/context).")
```
Add `var ownerFlag string` next to `var fleetAddr string`.

- [ ] **Step 3: Resolve + attach owner** — replace `dialFleet` so it resolves through axconfig and adds a client interceptor that appends the owner header:
```go
func ownerClientInterceptor(owner string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if owner != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, fleet.OwnerMetadataKey, owner)
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func dialFleet() (atefleetpb.FleetManagerClient, func(), error) {
	cfg, err := axconfig.Load()
	if err != nil {
		return nil, nil, err
	}
	addr, owner := cfg.Resolve(fleetAddr, ownerFlag)
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})),
		grpc.WithUnaryInterceptor(ownerClientInterceptor(owner)),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("dial atefleet %q: %w", addr, err)
	}
	return atefleetpb.NewFleetManagerClient(conn), func() { _ = conn.Close() }, nil
}
```

- [ ] **Step 4: Build + vet.** Run: `go build ./cmd/atefleet/... && go vet ./cmd/atefleet/...`. Expected: clean.
- [ ] **Step 5: Commit** — `git commit -am "atefleet: CLI resolves context + asserts owner metadata"`.

---

## Task 6: `ctx` subcommands

**Files:** Create `cmd/atefleet/ctx.go`; modify `cmd/atefleet/main.go`.

- [ ] **Step 1: Implement** `ctx.go`:
```go
// Copyright 2026 Google LLC  (Apache header)
package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/agent-substrate/substrate/cmd/atefleet/internal/axconfig"
)

func newCtxCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "ctx", Short: "Manage atefleet contexts (kubeconfig-style)"}
	cmd.AddCommand(newCtxListCmd(), newCtxUseCmd(), newCtxSetCmd())
	cmd.RunE = func(c *cobra.Command, _ []string) error { return runCtxList() } // bare `ctx` = list
	return cmd
}

func newCtxListCmd() *cobra.Command {
	return &cobra.Command{Use: "list", Short: "List contexts", RunE: func(_ *cobra.Command, _ []string) error { return runCtxList() }}
}

func runCtxList() error {
	cfg, err := axconfig.Load()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "CURRENT\tNAME\tFLEET-ADDR\tOWNER")
	for _, c := range cfg.Contexts {
		mark := ""
		if c.Name == cfg.CurrentContext {
			mark = "*"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", mark, c.Name, c.FleetAddr, c.Owner)
	}
	return w.Flush()
}

func newCtxUseCmd() *cobra.Command {
	return &cobra.Command{
		Use: "use <name>", Short: "Set the current context", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := axconfig.Load()
			if err != nil {
				return err
			}
			if err := cfg.Use(args[0]); err != nil {
				return err
			}
			fmt.Printf("switched to context %q\n", args[0])
			return nil
		},
	}
}

func newCtxSetCmd() *cobra.Command {
	var addr, owner string
	cmd := &cobra.Command{
		Use: "set <name>", Short: "Add or update a context", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := axconfig.Load()
			if err != nil {
				return err
			}
			if err := cfg.Set(axconfig.Context{Name: args[0], FleetAddr: addr, Owner: owner}); err != nil {
				return err
			}
			fmt.Printf("context %q set\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "fleet-addr", "", "FleetManager address for this context")
	cmd.Flags().StringVar(&owner, "owner", "", "Owner asserted by this context")
	return cmd
}
```

- [ ] **Step 2: Register** in `main.go` — add `newCtxCmd()` to the `root.AddCommand(...)` call.

- [ ] **Step 3: Build + vet + manual round-trip.** Run:
```bash
go build -o bin/atefleet ./cmd/atefleet && go vet ./cmd/atefleet/...
ATEFLEET_CONFIG=/tmp/axtest.yaml bin/atefleet ctx set alice --fleet-addr localhost:18443 --owner alice
ATEFLEET_CONFIG=/tmp/axtest.yaml bin/atefleet ctx list      # expect '*' on alice
```
Expected: builds clean; `ctx list` shows alice as current.

- [ ] **Step 4: Commit** — `git commit -am "atefleet: ctx list/use/set subcommands"`.

---

## Task 7: full build + test gate

- [ ] **Step 1:** `go build ./cmd/atefleet/...` — clean.
- [ ] **Step 2:** `go vet ./cmd/atefleet/...` — clean.
- [ ] **Step 3:** `go test -count=1 ./cmd/atefleet/...` — all PASS (axconfig + fleet incl. owner/scope tests).
- [ ] **Step 4: Commit** any remaining (should be none).

---

## Definition of done
- `axconfig` package: load/save/Active/Use/Set/Resolve, unit-tested.
- Owner metadata interceptor + `ownerFromCtx`, unit-tested.
- Handlers stamp (dispatch/run), force-filter (ls), and ownership-check (get/rm) by scope, unit-tested with the fake harness.
- `serve --require-owner` gates unscoped calls; default off (non-breaking).
- CLI resolves `{fleet-addr, owner}` via context precedence and asserts the owner header; `ctx list/use/set` work.
- `go build` + `go vet` + `go test ./cmd/atefleet/...` green.

## Deferred (out of scope)
- Verified tokens (option B: `token:` field + server `token→owner` map + `authorization: Bearer`).
- Group scoping; per-context TLS/credentials.
- Live-cluster e2e of scoped mode (can piggyback on the existing kind cluster: `serve --require-owner`, then `ctx set`/`use` + `dispatch`/`ls` as two owners).
