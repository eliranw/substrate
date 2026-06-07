# `atefleet` Phase 1 (MVP) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** A self-contained `atefleet` gRPC service (+ CLI) that dispatches and lists/gets/terminates Actors on agent-substrate, with per-Actor fleet metadata (role/owner/group/expiry) kept in `atefleet`'s own Redis index and a TTL reaper — using only `ateapi`'s **existing** API (no `ateapi`/proto changes).

**Architecture:** `atefleet` is a Deployment in `ate-system` exposing a `FleetManager` gRPC service. `DispatchActor` = `ateapi.CreateActor` + `ResumeActor` + write a `atefleet:actor:<id>` index record + return the `atenet` address. `ListFleet`/`GetFleetActor` join `ateapi.ListActors`/`GetActor` (source of truth for existence/state) with the index (metadata). `TerminateActor` = `ateapi.DeleteActor` + index delete. A reaper deletes expired Actors and reconciles stale index entries against `ListActors`.

**Tech Stack:** Go 1.26, gRPC + protobuf (`go generate`/`hack/protoc.sh`), `github.com/redis/go-redis/v9` (ClusterClient), `internal/ateclient` (Control client), `github.com/alicebob/miniredis/v2` (tests), cobra (CLI), `ko` (image), the existing `ate-system` deployment pattern.

**Spec:** `docs/superpowers/specs/2026-06-07-atefleet-design.md` (Phase 1; storage decision = **B**).

**Out of scope (later phases):** `RunSubtask`/`atefleet run` (Phase 2); count-quotas, `WatchFleet`, log lens, Claude-hook trigger (Phase 3); upstreaming an `Actor.labels` map (the eventual "A").

---

## Key facts this plan builds on (verbatim, from the codebase)

- **`ateapi` Control RPCs** (`pkg/proto/ateapipb/ateapi.proto`): `CreateActor(CreateActorRequest{actor_id, actor_template_namespace, actor_template_name}) → {Actor}`, `ResumeActor(ResumeActorRequest{actor_id, boot}) → {Actor}`, `GetActor(GetActorRequest{actor_id}) → {Actor}`, `DeleteActor(DeleteActorRequest{actor_id}) → {}`, `ListActors(ListActorsRequest{}) → {repeated Actor}`. `Actor{actor_id, version, actor_template_namespace, actor_template_name, status(enum), ateom_pod_*, last_snapshot, in_progress_snapshot, checkpoint_node}`.
- **Dial pattern** (`internal/ateclient/builder.go` `dialDirect`): `grpc.NewClient(endpoint, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify:true})), grpc.WithStatsHandler(otelgrpc.NewClientHandler()))` → `ateapipb.NewControlClient(conn)`. In-cluster ateapi Service = `api.ate-system.svc:443`.
- **Actor addressing** (`internal/resources/actor.go`): `ActorDNSSuffix = "actors.resources.substrate.ate.dev"`, `ValidateActorID(id)`. Address = `<actor_id> + "." + ActorDNSSuffix`.
- **Redis** (`cmd/ateapi/.../ateredis/ateredis.go`): `redis.ClusterClient`, key `actor:<id>`, value via `protojson`. We mirror with key `atefleet:actor:<id>` and JSON.
- **Proto gen** (`pkg/proto/ateapipb/gen.go`): a `//go:generate bash -c "../../../hack/protoc.sh --plugin=… --go_out=paths=source_relative:. --go-grpc_out=paths=source_relative:. <file>.proto"` directive; `go generate ./...`.
- **Manifest** (`manifests/ate-install/ate-api-server.yaml`): Deployment + SA + Service pattern (image `ko://…/cmd/<bin>`, gRPC on `:443` with a `servicedns.podcert.ate.dev` cred bundle, Service named in `ate-system`).

---

## File structure

- `internal/proto/atefleetpb/atefleet.proto` — **Create.** `FleetManager` service + messages.
- `internal/proto/atefleetpb/gen.go` + `*.pb.go` — **Create/Generated.**
- `cmd/atefleet/main.go` — **Create.** cobra root with `serve` + client subcommands.
- `cmd/atefleet/serve.go` — **Create.** wires Redis + ateapi client, starts the gRPC server.
- `cmd/atefleet/internal/fleet/meta.go` — **Create.** `FleetMeta` struct + Redis index store (`Put/Get/List/Delete`).
- `cmd/atefleet/internal/fleet/meta_test.go` — **Create.** index store tests (miniredis).
- `cmd/atefleet/internal/fleet/ateapi.go` — **Create.** the `ControlAPI` interface (subset of `ateapipb.ControlClient`) + a thin adapter; lets tests fake it.
- `cmd/atefleet/internal/fleet/address.go` + `address_test.go` — **Create.** address helper (TDD).
- `cmd/atefleet/internal/fleet/server.go` — **Create.** `FleetManager` gRPC impl (`DispatchActor`/`ListFleet`/`GetFleetActor`/`TerminateActor`).
- `cmd/atefleet/internal/fleet/server_test.go` — **Create.** handler tests (fake ControlAPI + miniredis).
- `cmd/atefleet/internal/fleet/reaper.go` + `reaper_test.go` — **Create.** TTL/stale reaper (TDD).
- `cmd/atefleet/cli.go` — **Create.** `dispatch`/`ls`/`get`/`rm` client subcommands.
- `manifests/ate-install/atefleet.yaml` — **Create.** Deployment + SA + Service.

---

## Task 1: `atefleetpb` proto + generation

**Files:** Create `internal/proto/atefleetpb/atefleet.proto`, `gen.go`; generate `*.pb.go`.

- [ ] **Step 1: Write the proto** — `internal/proto/atefleetpb/atefleet.proto` (Apache header like `internal/proto/atenodepb/atenode.proto`):
```proto
syntax = "proto3";
package atefleet;
option go_package = "github.com/agent-substrate/substrate/internal/proto/atefleetpb";

service FleetManager {
  rpc DispatchActor(DispatchActorRequest) returns (DispatchActorResponse) {}
  rpc ListFleet(ListFleetRequest) returns (ListFleetResponse) {}
  rpc GetFleetActor(GetFleetActorRequest) returns (GetFleetActorResponse) {}
  rpc TerminateActor(TerminateActorRequest) returns (TerminateActorResponse) {}
}

message DispatchActorRequest {
  string actor_template_namespace = 1;
  string actor_template_name      = 2;
  string actor_id                 = 3; // required for MVP (DNS-1123 label)
  string role                     = 4;
  string owner                    = 5;
  string group                    = 6;
  int64  ttl_seconds              = 7; // 0 = no expiry
}

message FleetActor {
  string actor_id    = 1;
  string address     = 2; // <actor_id>.actors.resources.substrate.ate.dev
  string status      = 3; // ateapi Actor.Status name, e.g. "STATUS_RUNNING"
  string role        = 4;
  string owner       = 5;
  string group       = 6;
  int64  expiry_unix = 7; // 0 = none
}

message DispatchActorResponse { FleetActor actor = 1; }
message ListFleetRequest  { string role = 1; string owner = 2; string group = 3; }
message ListFleetResponse { repeated FleetActor actors = 1; }
message GetFleetActorRequest  { string actor_id = 1; }
message GetFleetActorResponse { FleetActor actor = 1; }
message TerminateActorRequest  { string actor_id = 1; }
message TerminateActorResponse {}
```

- [ ] **Step 2: Add `gen.go`** mirroring `internal/proto/atenodepb/gen.go` exactly, substituting `atefleet.proto`:
```go
package atefleetpb

//go:generate bash -c "../../../hack/protoc.sh --plugin=protoc-gen-go=$(bash ../../../hack/run-tool.sh --print-bin-path protoc-gen-go) --plugin=protoc-gen-go-grpc=$(bash ../../../hack/run-tool.sh --print-bin-path protoc-gen-go-grpc) --go_out=paths=source_relative:. --go-grpc_out=paths=source_relative:. atefleet.proto"
```
(Add the repo's Apache license header above the package clause, matching `atenodepb/gen.go`.)

- [ ] **Step 3: Generate + build** — `cd internal/proto/atefleetpb && go generate ./...`; then `go build ./internal/proto/atefleetpb/...`. Expected: `atefleet.pb.go` + `atefleet_grpc.pb.go` created; build clean.

- [ ] **Step 4: Commit** — `git add internal/proto/atefleetpb && git commit -m "atefleet: FleetManager proto"`.

---

## Task 2: Fleet metadata index store (Redis) — TDD

**Files:** Create `cmd/atefleet/internal/fleet/meta.go`, `meta_test.go`.

- [ ] **Step 1: Failing test** — `meta_test.go` (uses miniredis like the repo's store tests):
```go
package fleet

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestIndex(t *testing.T) *Index {
	mr, err := miniredis.Run()
	if err != nil { t.Fatal(err) }
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return NewIndex(rdb)
}

func TestIndexPutGetListDelete(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)
	m := FleetMeta{ActorID: "a1", Role: "worker", Owner: "eliran", Group: "g1", ExpiryUnix: 123, TemplateNamespace: "ns", TemplateName: "tmpl"}
	if err := idx.Put(ctx, m); err != nil { t.Fatal(err) }

	got, err := idx.Get(ctx, "a1")
	if err != nil { t.Fatal(err) }
	if got.Owner != "eliran" || got.ExpiryUnix != 123 { t.Fatalf("got %+v", got) }

	all, err := idx.List(ctx)
	if err != nil { t.Fatal(err) }
	if len(all) != 1 || all[0].ActorID != "a1" { t.Fatalf("list = %+v", all) }

	if err := idx.Delete(ctx, "a1"); err != nil { t.Fatal(err) }
	if _, err := idx.Get(ctx, "a1"); err != ErrNotFound { t.Fatalf("want ErrNotFound, got %v", err) }
}
```

- [ ] **Step 2: Run → FAIL** (`undefined: Index/FleetMeta/...`): `go test ./cmd/atefleet/internal/fleet/ -run TestIndexPutGetListDelete -v`.

- [ ] **Step 3: Implement** `meta.go` (the redis client is an interface satisfied by both `*redis.Client` and `*redis.ClusterClient` — use `redis.Cmdable` + scan via `Keys`; key prefix `atefleet:actor:`):
```go
package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
)

var ErrNotFound = errors.New("fleet: not found")

const indexPrefix = "atefleet:actor:"

// FleetMeta is atefleet's per-Actor metadata, kept in its own Redis index.
// ateapi.ListActors remains the source of truth for existence/state.
type FleetMeta struct {
	ActorID           string `json:"actor_id"`
	Role              string `json:"role,omitempty"`
	Owner             string `json:"owner,omitempty"`
	Group             string `json:"group,omitempty"`
	ExpiryUnix        int64  `json:"expiry_unix,omitempty"`
	TemplateNamespace string `json:"template_namespace,omitempty"`
	TemplateName      string `json:"template_name,omitempty"`
}

type Index struct{ rdb redis.Cmdable }

func NewIndex(rdb redis.Cmdable) *Index { return &Index{rdb: rdb} }

func key(id string) string { return indexPrefix + id }

func (i *Index) Put(ctx context.Context, m FleetMeta) error {
	b, err := json.Marshal(m)
	if err != nil { return fmt.Errorf("marshal fleet meta: %w", err) }
	if err := i.rdb.Set(ctx, key(m.ActorID), b, 0).Err(); err != nil {
		return fmt.Errorf("redis set %q: %w", key(m.ActorID), err)
	}
	return nil
}

func (i *Index) Get(ctx context.Context, id string) (FleetMeta, error) {
	b, err := i.rdb.Get(ctx, key(id)).Bytes()
	if errors.Is(err, redis.Nil) { return FleetMeta{}, ErrNotFound }
	if err != nil { return FleetMeta{}, fmt.Errorf("redis get %q: %w", key(id), err) }
	var m FleetMeta
	if err := json.Unmarshal(b, &m); err != nil { return FleetMeta{}, fmt.Errorf("unmarshal: %w", err) }
	return m, nil
}

func (i *Index) List(ctx context.Context) ([]FleetMeta, error) {
	keys, err := i.rdb.Keys(ctx, indexPrefix+"*").Result()
	if err != nil { return nil, fmt.Errorf("redis keys: %w", err) }
	out := make([]FleetMeta, 0, len(keys))
	for _, k := range keys {
		b, err := i.rdb.Get(ctx, k).Bytes()
		if errors.Is(err, redis.Nil) { continue }
		if err != nil { return nil, fmt.Errorf("redis get %q: %w", k, err) }
		var m FleetMeta
		if err := json.Unmarshal(b, &m); err != nil { return nil, fmt.Errorf("unmarshal %q: %w", k, err) }
		out = append(out, m)
	}
	return out, nil
}

func (i *Index) Delete(ctx context.Context, id string) error {
	if err := i.rdb.Del(ctx, key(id)).Err(); err != nil { return fmt.Errorf("redis del %q: %w", key(id), err) }
	return nil
}
```
> Note: `Keys` is fine for the MVP/POC scale; switch to `SCAN` if the fleet grows large (flagged for Phase 3).

- [ ] **Step 4: Run → PASS.** **Step 5: Commit** — `git commit -am "atefleet: fleet metadata Redis index (TDD)"`.

---

## Task 3: `ControlAPI` interface + adapter (testable ateapi client)

**Files:** Create `cmd/atefleet/internal/fleet/ateapi.go`.

- [ ] **Step 1: Implement** the minimal interface `atefleet` uses + a thin adapter over the generated client (so handlers take an interface and tests fake it):
```go
package fleet

import (
	"context"

	ateapipb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// ControlAPI is the subset of ateapi's Control gRPC that atefleet uses.
type ControlAPI interface {
	CreateActor(ctx context.Context, in *ateapipb.CreateActorRequest) (*ateapipb.CreateActorResponse, error)
	ResumeActor(ctx context.Context, in *ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error)
	GetActor(ctx context.Context, in *ateapipb.GetActorRequest) (*ateapipb.GetActorResponse, error)
	ListActors(ctx context.Context, in *ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error)
	DeleteActor(ctx context.Context, in *ateapipb.DeleteActorRequest) (*ateapipb.DeleteActorResponse, error)
}

// controlAdapter wraps the generated ControlClient (which takes variadic
// grpc.CallOption) to satisfy ControlAPI.
type controlAdapter struct{ c ateapipb.ControlClient }

func NewControlAPI(c ateapipb.ControlClient) ControlAPI { return &controlAdapter{c: c} }

func (a *controlAdapter) CreateActor(ctx context.Context, in *ateapipb.CreateActorRequest) (*ateapipb.CreateActorResponse, error) { return a.c.CreateActor(ctx, in) }
func (a *controlAdapter) ResumeActor(ctx context.Context, in *ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error) { return a.c.ResumeActor(ctx, in) }
func (a *controlAdapter) GetActor(ctx context.Context, in *ateapipb.GetActorRequest) (*ateapipb.GetActorResponse, error) { return a.c.GetActor(ctx, in) }
func (a *controlAdapter) ListActors(ctx context.Context, in *ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) { return a.c.ListActors(ctx, in) }
func (a *controlAdapter) DeleteActor(ctx context.Context, in *ateapipb.DeleteActorRequest) (*ateapipb.DeleteActorResponse, error) { return a.c.DeleteActor(ctx, in) }
```

- [ ] **Step 2: Build** `go build ./cmd/atefleet/...`. **Step 3: Commit.**

---

## Task 4: Address helper — TDD

**Files:** Create `cmd/atefleet/internal/fleet/address.go`, `address_test.go`.

- [ ] **Step 1: Failing test:**
```go
package fleet

import "testing"

func TestActorAddress(t *testing.T) {
	got, err := ActorAddress("counter1")
	if err != nil { t.Fatal(err) }
	if got != "counter1.actors.resources.substrate.ate.dev" { t.Fatalf("got %q", got) }
	if _, err := ActorAddress("Bad_ID"); err == nil { t.Fatal("want error for invalid id") }
}
```

- [ ] **Step 2: Run → FAIL.** **Step 3: Implement** `address.go`:
```go
package fleet

import (
	"fmt"

	"github.com/agent-substrate/substrate/internal/resources"
)

// ActorAddress returns the atenet-routable hostname for an actor id.
func ActorAddress(actorID string) (string, error) {
	if err := resources.ValidateActorID(actorID); err != nil {
		return "", fmt.Errorf("invalid actor id: %w", err)
	}
	return actorID + "." + resources.ActorDNSSuffix, nil
}
```
- [ ] **Step 4: Run → PASS.** **Step 5: Commit.**

---

## Task 5: `DispatchActor` handler — TDD

**Files:** Create `cmd/atefleet/internal/fleet/server.go` (the `Server` + `DispatchActor`); `server_test.go` (fake `ControlAPI` + miniredis).

- [ ] **Step 1: Failing test** — `server_test.go` (a fake ControlAPI recording calls):
```go
package fleet

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	atefleetpb "github.com/agent-substrate/substrate/internal/proto/atefleetpb"
	ateapipb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

type fakeControl struct {
	created, resumed, deleted []string
	actors                    []*ateapipb.Actor
}
func (f *fakeControl) CreateActor(_ context.Context, in *ateapipb.CreateActorRequest) (*ateapipb.CreateActorResponse, error) {
	f.created = append(f.created, in.GetActorId())
	a := &ateapipb.Actor{ActorId: in.GetActorId(), ActorTemplateNamespace: in.GetActorTemplateNamespace(), ActorTemplateName: in.GetActorTemplateName(), Status: ateapipb.Actor_STATUS_SUSPENDED}
	f.actors = append(f.actors, a)
	return &ateapipb.CreateActorResponse{Actor: a}, nil
}
func (f *fakeControl) ResumeActor(_ context.Context, in *ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error) {
	f.resumed = append(f.resumed, in.GetActorId())
	for _, a := range f.actors { if a.ActorId == in.GetActorId() { a.Status = ateapipb.Actor_STATUS_RUNNING; return &ateapipb.ResumeActorResponse{Actor: a}, nil } }
	return &ateapipb.ResumeActorResponse{}, nil
}
func (f *fakeControl) GetActor(_ context.Context, in *ateapipb.GetActorRequest) (*ateapipb.GetActorResponse, error) {
	for _, a := range f.actors { if a.ActorId == in.GetActorId() { return &ateapipb.GetActorResponse{Actor: a}, nil } }
	return nil, ErrNotFound
}
func (f *fakeControl) ListActors(_ context.Context, _ *ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
	return &ateapipb.ListActorsResponse{Actors: f.actors}, nil
}
func (f *fakeControl) DeleteActor(_ context.Context, in *ateapipb.DeleteActorRequest) (*ateapipb.DeleteActorResponse, error) {
	f.deleted = append(f.deleted, in.GetActorId())
	kept := f.actors[:0]; for _, a := range f.actors { if a.ActorId != in.GetActorId() { kept = append(kept, a) } }; f.actors = kept
	return &ateapipb.DeleteActorResponse{}, nil
}

func newTestServer(t *testing.T) (*Server, *fakeControl) {
	mr, err := miniredis.Run(); if err != nil { t.Fatal(err) }; t.Cleanup(mr.Close)
	idx := NewIndex(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	fc := &fakeControl{}
	return NewServer(fc, idx, func() int64 { return 1000 }), fc // nowUnix fixed at 1000
}

func TestDispatchActor(t *testing.T) {
	s, fc := newTestServer(t)
	resp, err := s.DispatchActor(context.Background(), &atefleetpb.DispatchActorRequest{
		ActorTemplateNamespace: "ns", ActorTemplateName: "tmpl", ActorId: "a1",
		Role: "worker", Owner: "eliran", Group: "g1", TtlSeconds: 60,
	})
	if err != nil { t.Fatal(err) }
	if resp.GetActor().GetAddress() != "a1.actors.resources.substrate.ate.dev" { t.Fatalf("addr %q", resp.GetActor().GetAddress()) }
	if len(fc.created) != 1 || len(fc.resumed) != 1 { t.Fatalf("create=%v resume=%v", fc.created, fc.resumed) }
	m, err := s.idx.Get(context.Background(), "a1"); if err != nil { t.Fatal(err) }
	if m.Owner != "eliran" || m.ExpiryUnix != 1060 { t.Fatalf("meta %+v", m) } // now(1000)+ttl(60)
}
```

- [ ] **Step 2: Run → FAIL.** **Step 3: Implement** `server.go` (`Server` + `DispatchActor`):
```go
package fleet

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	atefleetpb "github.com/agent-substrate/substrate/internal/proto/atefleetpb"
	ateapipb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

type Server struct {
	atefleetpb.UnimplementedFleetManagerServer
	api ControlAPI
	idx *Index
	now func() int64 // unix seconds; injectable for tests
}

func NewServer(api ControlAPI, idx *Index, now func() int64) *Server {
	return &Server{api: api, idx: idx, now: now}
}

func (s *Server) DispatchActor(ctx context.Context, r *atefleetpb.DispatchActorRequest) (*atefleetpb.DispatchActorResponse, error) {
	addr, err := ActorAddress(r.GetActorId())
	if err != nil { return nil, status.Errorf(codes.InvalidArgument, "%v", err) }
	if r.GetActorTemplateNamespace() == "" || r.GetActorTemplateName() == "" {
		return nil, status.Error(codes.InvalidArgument, "actor_template_namespace and actor_template_name are required")
	}

	if _, err := s.api.CreateActor(ctx, &ateapipb.CreateActorRequest{
		ActorId: r.GetActorId(), ActorTemplateNamespace: r.GetActorTemplateNamespace(), ActorTemplateName: r.GetActorTemplateName(),
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "create actor: %v", err)
	}
	resumeResp, err := s.api.ResumeActor(ctx, &ateapipb.ResumeActorRequest{ActorId: r.GetActorId()})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resume actor: %v", err)
	}

	var expiry int64
	if r.GetTtlSeconds() > 0 { expiry = s.now() + r.GetTtlSeconds() }
	meta := FleetMeta{
		ActorID: r.GetActorId(), Role: r.GetRole(), Owner: r.GetOwner(), Group: r.GetGroup(),
		ExpiryUnix: expiry, TemplateNamespace: r.GetActorTemplateNamespace(), TemplateName: r.GetActorTemplateName(),
	}
	if err := s.idx.Put(ctx, meta); err != nil {
		return nil, status.Errorf(codes.Internal, "index actor: %v", err)
	}

	return &atefleetpb.DispatchActorResponse{Actor: fleetActor(meta, addr, resumeResp.GetActor().GetStatus())}, nil
}

func fleetActor(m FleetMeta, addr string, st ateapipb.Actor_Status) *atefleetpb.FleetActor {
	return &atefleetpb.FleetActor{
		ActorId: m.ActorID, Address: addr, Status: st.String(),
		Role: m.Role, Owner: m.Owner, Group: m.Group, ExpiryUnix: m.ExpiryUnix,
	}
}
```
> Expose `s.idx` (unexported field accessed in same-package test) — fine since `server_test.go` is in `package fleet`.

- [ ] **Step 4: Run → PASS.** **Step 5: Commit** — `git commit -am "atefleet: DispatchActor handler (TDD)"`.

---

## Task 6: `ListFleet` + `GetFleetActor` — TDD

**Files:** add to `server.go`, `server_test.go`.

- [ ] **Step 1: Failing test** (dispatch two actors with different groups; list filtered):
```go
func TestListFleetFilter(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()
	for _, id := range []string{"a1", "a2"} {
		grp := "g1"; if id == "a2" { grp = "g2" }
		if _, err := s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace:"ns", ActorTemplateName:"t", ActorId:id, Group:grp}); err != nil { t.Fatal(err) }
	}
	resp, err := s.ListFleet(ctx, &atefleetpb.ListFleetRequest{Group: "g2"})
	if err != nil { t.Fatal(err) }
	if len(resp.GetActors()) != 1 || resp.GetActors()[0].GetActorId() != "a2" { t.Fatalf("got %+v", resp.GetActors()) }
	if resp.GetActors()[0].GetStatus() != "STATUS_RUNNING" { t.Fatalf("status %q", resp.GetActors()[0].GetStatus()) }

	g, err := s.GetFleetActor(ctx, &atefleetpb.GetFleetActorRequest{ActorId:"a1"})
	if err != nil { t.Fatal(err) }
	if g.GetActor().GetAddress() != "a1.actors.resources.substrate.ate.dev" { t.Fatalf("addr %q", g.GetActor().GetAddress()) }
}
```

- [ ] **Step 2: Run → FAIL.** **Step 3: Implement** (join ListActors (existence/state) with the index (metadata); filter):
```go
func (s *Server) ListFleet(ctx context.Context, r *atefleetpb.ListFleetRequest) (*atefleetpb.ListFleetResponse, error) {
	metas, err := s.idx.List(ctx)
	if err != nil { return nil, status.Errorf(codes.Internal, "list index: %v", err) }
	la, err := s.api.ListActors(ctx, &ateapipb.ListActorsRequest{})
	if err != nil { return nil, status.Errorf(codes.Internal, "list actors: %v", err) }
	stByID := map[string]ateapipb.Actor_Status{}
	for _, a := range la.GetActors() { stByID[a.GetActorId()] = a.GetStatus() }

	out := &atefleetpb.ListFleetResponse{}
	for _, m := range metas {
		if r.GetRole() != "" && m.Role != r.GetRole() { continue }
		if r.GetOwner() != "" && m.Owner != r.GetOwner() { continue }
		if r.GetGroup() != "" && m.Group != r.GetGroup() { continue }
		st, live := stByID[m.ActorID]
		if !live { continue } // actor gone but index lingering — skip (reaper cleans it)
		addr, _ := ActorAddress(m.ActorID)
		out.Actors = append(out.Actors, fleetActor(m, addr, st))
	}
	return out, nil
}

func (s *Server) GetFleetActor(ctx context.Context, r *atefleetpb.GetFleetActorRequest) (*atefleetpb.GetFleetActorResponse, error) {
	m, err := s.idx.Get(ctx, r.GetActorId())
	if err == ErrNotFound { return nil, status.Error(codes.NotFound, "actor not in fleet") }
	if err != nil { return nil, status.Errorf(codes.Internal, "get index: %v", err) }
	ga, err := s.api.GetActor(ctx, &ateapipb.GetActorRequest{ActorId: r.GetActorId()})
	if err != nil { return nil, status.Errorf(codes.Internal, "get actor: %v", err) }
	addr, _ := ActorAddress(m.ActorID)
	return &atefleetpb.GetFleetActorResponse{Actor: fleetActor(m, addr, ga.GetActor().GetStatus())}, nil
}
```
- [ ] **Step 4: Run → PASS.** **Step 5: Commit.**

---

## Task 7: `TerminateActor` — TDD

**Files:** add to `server.go`, `server_test.go`.

- [ ] **Step 1: Failing test:**
```go
func TestTerminateActor(t *testing.T) {
	s, fc := newTestServer(t)
	ctx := context.Background()
	s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace:"ns", ActorTemplateName:"t", ActorId:"a1"})
	if _, err := s.TerminateActor(ctx, &atefleetpb.TerminateActorRequest{ActorId:"a1"}); err != nil { t.Fatal(err) }
	if len(fc.deleted) != 1 || fc.deleted[0] != "a1" { t.Fatalf("deleted %v", fc.deleted) }
	if _, err := s.idx.Get(ctx, "a1"); err != ErrNotFound { t.Fatal("index entry should be gone") }
}
```

- [ ] **Step 2: Run → FAIL.** **Step 3: Implement:**
```go
func (s *Server) TerminateActor(ctx context.Context, r *atefleetpb.TerminateActorRequest) (*atefleetpb.TerminateActorResponse, error) {
	if _, err := s.api.DeleteActor(ctx, &ateapipb.DeleteActorRequest{ActorId: r.GetActorId()}); err != nil {
		return nil, status.Errorf(codes.Internal, "delete actor: %v", err)
	}
	if err := s.idx.Delete(ctx, r.GetActorId()); err != nil {
		return nil, status.Errorf(codes.Internal, "delete index: %v", err)
	}
	return &atefleetpb.TerminateActorResponse{}, nil
}
```
> Note: `ateapi.DeleteActor` only deletes **suspended** actors. Phase 2 may add suspend-then-delete; for MVP, `TerminateActor` surfaces the `ateapi` error if the actor isn't suspended (acceptable — caller suspends first).

- [ ] **Step 4: Run → PASS.** **Step 5: Commit.**

---

## Task 8: TTL/stale reaper — TDD

**Files:** Create `cmd/atefleet/internal/fleet/reaper.go`, `reaper_test.go`.

- [ ] **Step 1: Failing test** (one expired actor, one live, one stale-index-only):
```go
package fleet

import (
	"context"
	"testing"

	atefleetpb "github.com/agent-substrate/substrate/internal/proto/atefleetpb"
)

func TestReapOnce(t *testing.T) {
	s, fc := newTestServer(t) // now() == 1000
	ctx := context.Background()
	// expired: ttl makes expiry 1000+1=1001 but we'll reap at now=2000
	s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace:"ns",ActorTemplateName:"t",ActorId:"old", TtlSeconds:1})
	// live, no ttl
	s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace:"ns",ActorTemplateName:"t",ActorId:"keep"})
	// stale index entry (actor not in ateapi)
	s.idx.Put(ctx, FleetMeta{ActorID:"ghost"})

	r := NewReaper(fc, s.idx, func() int64 { return 2000 })
	if err := r.ReapOnce(ctx); err != nil { t.Fatal(err) }

	if len(fc.deleted) != 1 || fc.deleted[0] != "old" { t.Fatalf("deleted %v (want [old])", fc.deleted) }
	if _, err := s.idx.Get(ctx, "old"); err != ErrNotFound { t.Fatal("old index not cleared") }
	if _, err := s.idx.Get(ctx, "ghost"); err != ErrNotFound { t.Fatal("ghost index not reconciled") }
	if _, err := s.idx.Get(ctx, "keep"); err != nil { t.Fatal("keep should remain") }
}
```

- [ ] **Step 2: Run → FAIL.** **Step 3: Implement** `reaper.go`:
```go
package fleet

import (
	"context"
	"fmt"
	"log/slog"

	ateapipb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

type Reaper struct {
	api ControlAPI
	idx *Index
	now func() int64
}

func NewReaper(api ControlAPI, idx *Index, now func() int64) *Reaper {
	return &Reaper{api: api, idx: idx, now: now}
}

// ReapOnce: delete expired actors; drop index entries whose actor no longer exists.
func (r *Reaper) ReapOnce(ctx context.Context) error {
	metas, err := r.idx.List(ctx)
	if err != nil { return fmt.Errorf("list index: %w", err) }
	la, err := r.api.ListActors(ctx, &ateapipb.ListActorsRequest{})
	if err != nil { return fmt.Errorf("list actors: %w", err) }
	live := map[string]bool{}
	for _, a := range la.GetActors() { live[a.GetActorId()] = true }

	now := r.now()
	for _, m := range metas {
		switch {
		case !live[m.ActorID]:
			// actor gone (deleted out-of-band) — reconcile the stale index entry
			if err := r.idx.Delete(ctx, m.ActorID); err != nil { return err }
			slog.InfoContext(ctx, "reaper: dropped stale index entry", "actor", m.ActorID)
		case m.ExpiryUnix > 0 && now >= m.ExpiryUnix:
			if _, err := r.api.DeleteActor(ctx, &ateapipb.DeleteActorRequest{ActorId: m.ActorID}); err != nil {
				slog.WarnContext(ctx, "reaper: delete expired actor failed", "actor", m.ActorID, "err", err)
				continue // keep index; retry next tick
			}
			if err := r.idx.Delete(ctx, m.ActorID); err != nil { return err }
			slog.InfoContext(ctx, "reaper: reaped expired actor", "actor", m.ActorID)
		}
	}
	return nil
}
```
> Note: deleting an expired actor requires it to be suspended (per `ateapi.DeleteActor`); if it's running, the delete fails and we retry next tick. Phase 2 (suspend-then-delete) closes this; acceptable for MVP.

- [ ] **Step 4: Run → PASS.** **Step 5: Commit.**

---

## Task 9: `serve` wiring (Redis + ateapi client + gRPC server + reaper loop)

**Files:** Create `cmd/atefleet/serve.go` (+ referenced by `main.go` in Task 10).

- [ ] **Step 1: Implement** `serve.go` — flags mirror `ateapi` (`--grpc-listen-addr`, `--ateapi-addr`, `--redis-cluster-address`, redis TLS flags), dial ateapi via `internal/ateclient` (or the `dialDirect` pattern), connect Redis (`redis.NewClusterClient` like `cmd/ateapi/main.go`), construct `fleet.Server` + `fleet.Reaper`, start a reaper ticker, serve gRPC:
```go
package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/agent-substrate/substrate/cmd/atefleet/internal/fleet"
	atefleetpb "github.com/agent-substrate/substrate/internal/proto/atefleetpb"
	ateapipb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

func newServeCmd() *cobra.Command {
	var listen, ateapiAddr, redisAddr string
	var reapEvery time.Duration
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the atefleet FleetManager gRPC service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			// ateapi Control client (TLS; matches internal/ateclient.dialDirect)
			conn, err := grpc.NewClient(ateapiAddr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
			if err != nil { return err }
			api := fleet.NewControlAPI(ateapipb.NewControlClient(conn))

			rdb := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{redisAddr}})
			if err := rdb.Ping(ctx).Err(); err != nil { return err }
			idx := fleet.NewIndex(rdb)
			nowUnix := func() int64 { return time.Now().Unix() }

			srv := fleet.NewServer(api, idx, nowUnix)
			reaper := fleet.NewReaper(api, idx, nowUnix)
			go runReaper(ctx, reaper, reapEvery)

			lis, err := net.Listen("tcp", listen); if err != nil { return err }
			g := grpc.NewServer()
			atefleetpb.RegisterFleetManagerServer(g, srv)
			slog.InfoContext(ctx, "atefleet serving", "addr", listen)
			return g.Serve(lis)
		},
	}
	cmd.Flags().StringVar(&listen, "grpc-listen-addr", "0.0.0.0:443", "")
	cmd.Flags().StringVar(&ateapiAddr, "ateapi-addr", "api.ate-system.svc:443", "")
	cmd.Flags().StringVar(&redisAddr, "redis-cluster-address", "", "")
	cmd.Flags().DurationVar(&reapEvery, "reap-interval", 30*time.Second, "")
	return cmd
}

func runReaper(ctx context.Context, r *fleet.Reaper, every time.Duration) {
	t := time.NewTicker(every); defer t.Stop()
	for {
		select {
		case <-ctx.Done(): return
		case <-t.C:
			if err := r.ReapOnce(ctx); err != nil { slog.WarnContext(ctx, "reaper tick failed", "err", err) }
		}
	}
}
```
> Auth note (resolve during impl): `ateapi` authenticates in-cluster clients via its `servicedns.podcert` mTLS / client-JWT. Mirror how `atenet`/`internal/ateclient` authenticate (the manifest mounts the same cred bundle); the `InsecureSkipVerify` transport here matches `dialDirect` but client identity (podcert/JWT) must match `ateapi`'s expectations — confirm against a live `ateapi` and add the cred-bundle dial option + manifest volume if required. Redis TLS flags likewise mirror `cmd/ateapi/main.go`'s `buildRedisTLSConfig`.

- [ ] **Step 2: Build** `go build ./cmd/atefleet/...`. **Step 3: Commit.**

---

## Task 10: `main.go` + CLI subcommands

**Files:** Create `cmd/atefleet/main.go`, `cmd/atefleet/cli.go`.

- [ ] **Step 1:** `main.go` — cobra root wiring `serve` (Task 9) + the client subcommands (Task 10 below):
```go
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	root := newRootCmd()
	root.AddCommand(newServeCmd(), newDispatchCmd(), newLsCmd(), newGetCmd(), newRmCmd())
	if err := root.ExecuteContext(ctx); err != nil { slog.Error("atefleet", "err", err); os.Exit(1) }
}
```
- [ ] **Step 2:** `cli.go` — `newRootCmd` (with a `--fleet-addr` persistent flag, default `atefleet.ate-system.svc:443`) and the four client subcommands that dial the FleetManager (same TLS pattern) and call `DispatchActor`/`ListFleet`/`GetFleetActor`/`TerminateActor`, printing a table. (Mirror `cmd/kubectl-ate` cobra + client-dial structure; each subcommand: dial `--fleet-addr`, build the request from flags/args, print the response.)
- [ ] **Step 3: Build** `go build ./cmd/atefleet/...`; `go vet ./cmd/atefleet/...`. **Step 4: Commit.**

---

## Task 11: Deployment + SA + Service manifest

**Files:** Create `manifests/ate-install/atefleet.yaml`.

- [ ] **Step 1:** Author `atefleet.yaml` modeled on `ate-api-server.yaml` (SA `atefleet`; Deployment `atefleet-deployment` in `ate-system`, image `ko://github.com/agent-substrate/substrate/cmd/atefleet`, args `serve --grpc-listen-addr=0.0.0.0:443 --ateapi-addr=api.ate-system.svc:443 --redis-cluster-address=@env` + redis TLS args mirroring `ate-api-server.yaml`; the `servicedns.podcert` + `valkey-ca` projected volumes/mounts copied from `ate-api-server.yaml`; Service `atefleet` in `ate-system`, ClusterIP, port 443→443). **No ClusterRole needed** — `atefleet` makes no Kubernetes API calls (it uses `ateapi` gRPC + Redis), so only the SA.
- [ ] **Step 2:** `kubectl apply --dry-run=client -f manifests/ate-install/atefleet.yaml` (offline) to validate YAML. **Step 3: Commit.**

---

## Task 12: integration smoke (deferred to a gate)

- [ ] On a substrate cluster with `ateapi` reachable: `ko apply -f manifests/ate-install/atefleet.yaml`; `atefleet dispatch --template ate-demo/counter --id c1 --owner eliran --ttl 300s`; confirm an Actor `c1` is created+running and `atefleet ls --owner eliran` shows it with address `c1.actors.resources.substrate.ate.dev`; `atefleet rm c1` (suspend first if needed) removes it + the index entry; verify the reaper drops it after TTL. Record the verdict. *(This gate may piggyback on the native-pod substrate stood up for the gVisor POC.)*

---

## Definition of done (Phase 1)

- `atefleetpb` proto generated; `atefleet` binary builds.
- `DispatchActor`/`ListFleet`/`GetFleetActor`/`TerminateActor` implemented + unit-tested against a fake `ateapi` + miniredis.
- TTL/stale reaper implemented + tested.
- `serve` wiring (Redis + ateapi client + gRPC + reaper loop) + CLI subcommands build.
- Deployment/SA/Service manifest authored + dry-run-valid.

## Deferred follow-ups

- Phase 2: `RunSubtask` + `atefleet run` (one-shot offload).
- Phase 3: count-quotas, `WatchFleet`, log lens, Claude-hook trigger; switch `Keys`→`SCAN`.
- Upstream "A" (`Actor.labels` map) + migrate `atefleet` off its private index.
- Resolve `ateapi` client auth (podcert mTLS / client-JWT) precisely (Task 9 note).
- `TerminateActor`/reaper: suspend-then-delete so running actors can be reaped.
