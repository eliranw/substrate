// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fleet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	atefleetpb "github.com/agent-substrate/substrate/internal/proto/atefleetpb"
	ateapipb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

type fakeControl struct {
	created, resumed, suspended, deleted []string
	actors                               []*ateapipb.Actor
	createErr                            error // if set, CreateActor returns it before mutating state
	resumeErr                            error // if set, ResumeActor returns it before mutating state
	deleteErr                            error // if set, DeleteActor returns it before mutating state
	getErr                               error // if set, GetActor returns it instead of looking up actors
	// pageSize, when >0, makes ListActors return at most this many actors per
	// call and emit a NextPageToken so multi-page pagination can be exercised.
	pageSize int
}

func (f *fakeControl) CreateActor(_ context.Context, in *ateapipb.CreateActorRequest) (*ateapipb.CreateActorResponse, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, in.GetActorId())
	a := &ateapipb.Actor{ActorId: in.GetActorId(), ActorTemplateNamespace: in.GetActorTemplateNamespace(), ActorTemplateName: in.GetActorTemplateName(), Status: ateapipb.Actor_STATUS_SUSPENDED}
	f.actors = append(f.actors, a)
	return &ateapipb.CreateActorResponse{Actor: a}, nil
}
func (f *fakeControl) ResumeActor(_ context.Context, in *ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error) {
	if f.resumeErr != nil {
		return nil, f.resumeErr
	}
	f.resumed = append(f.resumed, in.GetActorId())
	for _, a := range f.actors {
		if a.ActorId == in.GetActorId() {
			a.Status = ateapipb.Actor_STATUS_RUNNING
			// A resumed actor reports the worker pod IP; RunSubtask needs it.
			a.AteomPodIp = "10.0.0.1"
			return &ateapipb.ResumeActorResponse{Actor: a}, nil
		}
	}
	return &ateapipb.ResumeActorResponse{}, nil
}
func (f *fakeControl) SuspendActor(_ context.Context, in *ateapipb.SuspendActorRequest) (*ateapipb.SuspendActorResponse, error) {
	f.suspended = append(f.suspended, in.GetActorId())
	for _, a := range f.actors {
		if a.ActorId == in.GetActorId() {
			a.Status = ateapipb.Actor_STATUS_SUSPENDED
			return &ateapipb.SuspendActorResponse{Actor: a}, nil
		}
	}
	return &ateapipb.SuspendActorResponse{}, nil
}
func (f *fakeControl) GetActor(_ context.Context, in *ateapipb.GetActorRequest) (*ateapipb.GetActorResponse, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	for _, a := range f.actors {
		if a.ActorId == in.GetActorId() {
			return &ateapipb.GetActorResponse{Actor: a}, nil
		}
	}
	// Mirror ateapi: a missing actor surfaces as a gRPC NotFound status.
	return nil, status.Error(codes.NotFound, "actor not found")
}
func (f *fakeControl) ListActors(_ context.Context, in *ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
	if f.pageSize <= 0 {
		return &ateapipb.ListActorsResponse{Actors: f.actors}, nil
	}
	// Paginated mode: PageToken is the 1-based start offset encoded as decimal.
	start := 0
	if in.GetPageToken() != "" {
		if _, err := fmt.Sscanf(in.GetPageToken(), "%d", &start); err != nil {
			return nil, fmt.Errorf("bad page token %q: %w", in.GetPageToken(), err)
		}
	}
	end := start + f.pageSize
	if end > len(f.actors) {
		end = len(f.actors)
	}
	resp := &ateapipb.ListActorsResponse{Actors: f.actors[start:end]}
	if end < len(f.actors) {
		resp.NextPageToken = fmt.Sprintf("%d", end)
	}
	return resp, nil
}
func (f *fakeControl) DeleteActor(_ context.Context, in *ateapipb.DeleteActorRequest) (*ateapipb.DeleteActorResponse, error) {
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	// Faithful to ateapi.DeleteActor: a running (not suspended) actor cannot be
	// deleted and surfaces FailedPrecondition. The earlier lenient fake hid the
	// bug where TerminateActor/the reaper deleted without suspending first.
	for _, a := range f.actors {
		if a.ActorId == in.GetActorId() && a.Status != ateapipb.Actor_STATUS_SUSPENDED {
			return nil, status.Errorf(codes.FailedPrecondition, "Actor %s is not suspended (status: %v)", in.GetActorId(), a.Status)
		}
	}
	f.deleted = append(f.deleted, in.GetActorId())
	kept := f.actors[:0]
	for _, a := range f.actors {
		if a.ActorId != in.GetActorId() {
			kept = append(kept, a)
		}
	}
	f.actors = kept
	return &ateapipb.DeleteActorResponse{}, nil
}

// fakeRunner records the address and command it was asked to run and returns a
// canned result (or error) so RunSubtask can be exercised without a worker pod.
type fakeRunner struct {
	gotAddr    string
	gotCommand []string
	gotTimeout time.Duration
	calls      int
	res        RunResult
	err        error
}

func (r *fakeRunner) Run(_ context.Context, addr string, command []string, timeout time.Duration) (RunResult, error) {
	r.calls++
	r.gotAddr = addr
	r.gotCommand = command
	r.gotTimeout = timeout
	return r.res, r.err
}

func newTestServer(t *testing.T) (*Server, *fakeControl, *fakeRunner) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	idx := NewIndex(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	fc := &fakeControl{}
	fr := &fakeRunner{}
	return NewServer(fc, idx, func() int64 { return 1000 }, fr), fc, fr
}

func TestDispatchActor(t *testing.T) {
	s, fc, _ := newTestServer(t)
	resp, err := s.DispatchActor(context.Background(), &atefleetpb.DispatchActorRequest{
		ActorTemplateNamespace: "ns", ActorTemplateName: "tmpl", ActorId: "a1",
		Role: "worker", Owner: "eliran", Group: "g1", TtlSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetActor().GetAddress() != "a1.actors.resources.substrate.ate.dev" {
		t.Fatalf("addr %q", resp.GetActor().GetAddress())
	}
	if len(fc.created) != 1 || len(fc.resumed) != 1 {
		t.Fatalf("create=%v resume=%v", fc.created, fc.resumed)
	}
	m, err := s.idx.Get(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Owner != "eliran" || m.ExpiryUnix != 1060 {
		t.Fatalf("meta %+v", m)
	}
}

func TestDispatchActorPreservesCreateCode(t *testing.T) {
	s, fc, _ := newTestServer(t)
	fc.createErr = status.Error(codes.AlreadyExists, "Actor a1 already exists")
	_, err := s.DispatchActor(context.Background(), &atefleetpb.DispatchActorRequest{
		ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: "a1",
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("want AlreadyExists, got %v (%v)", status.Code(err), err)
	}
}

func TestDispatchActorPreservesResumeCode(t *testing.T) {
	s, fc, _ := newTestServer(t)
	fc.resumeErr = status.Error(codes.Aborted, "Actor a1 is being reconciled")
	_, err := s.DispatchActor(context.Background(), &atefleetpb.DispatchActorRequest{
		ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: "a1",
	})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("want Aborted, got %v (%v)", status.Code(err), err)
	}
}

func TestDispatchActorInvalidArgs(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := context.Background()
	// empty template name
	if _, err := s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace: "ns", ActorId: "a1"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty template name: want InvalidArgument, got %v (%v)", status.Code(err), err)
	}
	// invalid actor id
	if _, err := s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: "Bad_ID!"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid actor id: want InvalidArgument, got %v (%v)", status.Code(err), err)
	}
}

func TestListFleetFilter(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := context.Background()
	for _, id := range []string{"a1", "a2"} {
		grp := "g1"
		if id == "a2" {
			grp = "g2"
		}
		if _, err := s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: id, Group: grp}); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := s.ListFleet(ctx, &atefleetpb.ListFleetRequest{Group: "g2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetActors()) != 1 || resp.GetActors()[0].GetActorId() != "a2" {
		t.Fatalf("got %+v", resp.GetActors())
	}
	if resp.GetActors()[0].GetStatus() != "STATUS_RUNNING" {
		t.Fatalf("status %q", resp.GetActors()[0].GetStatus())
	}

	g, err := s.GetFleetActor(ctx, &atefleetpb.GetFleetActorRequest{ActorId: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if g.GetActor().GetAddress() != "a1.actors.resources.substrate.ate.dev" {
		t.Fatalf("addr %q", g.GetActor().GetAddress())
	}
}

func TestListFleetFilterByRoleAndOwner(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := context.Background()
	dispatch := func(id, role, owner string) {
		if _, err := s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{
			ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: id, Role: role, Owner: owner,
		}); err != nil {
			t.Fatal(err)
		}
	}
	dispatch("a1", "worker", "alice")
	dispatch("a2", "worker", "bob")
	dispatch("a3", "leader", "alice")

	roleResp, err := s.ListFleet(ctx, &atefleetpb.ListFleetRequest{Role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if got := actorIDs(roleResp.GetActors()); !sameSet(got, []string{"a1", "a2"}) {
		t.Fatalf("filter by role: got %v want [a1 a2]", got)
	}

	ownerResp, err := s.ListFleet(ctx, &atefleetpb.ListFleetRequest{Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if got := actorIDs(ownerResp.GetActors()); !sameSet(got, []string{"a1", "a3"}) {
		t.Fatalf("filter by owner: got %v want [a1 a3]", got)
	}
}

func TestListFleetOmitsActorAbsentFromListActors(t *testing.T) {
	s, fc, _ := newTestServer(t)
	ctx := context.Background()
	if _, err := s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: "a1"}); err != nil {
		t.Fatal(err)
	}
	// Index entry survives, but ateapi no longer lists the actor (deleted out-of-band).
	fc.actors = nil
	resp, err := s.ListFleet(ctx, &atefleetpb.ListFleetRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetActors()) != 0 {
		t.Fatalf("expected absent actor omitted, got %+v", resp.GetActors())
	}
}

func TestGetFleetActorFields(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := context.Background()
	if _, err := s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{
		ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: "a1",
		Role: "worker", Owner: "alice", Group: "g1", TtlSeconds: 60,
	}); err != nil {
		t.Fatal(err)
	}
	g, err := s.GetFleetActor(ctx, &atefleetpb.GetFleetActorRequest{ActorId: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	a := g.GetActor()
	if a.GetStatus() != "STATUS_RUNNING" {
		t.Fatalf("status %q", a.GetStatus())
	}
	if a.GetRole() != "worker" || a.GetOwner() != "alice" || a.GetGroup() != "g1" {
		t.Fatalf("role/owner/group %q/%q/%q", a.GetRole(), a.GetOwner(), a.GetGroup())
	}
	if a.GetExpiryUnix() != 1060 {
		t.Fatalf("expiry %d, want 1060", a.GetExpiryUnix())
	}
}

func TestGetFleetActorActorGone(t *testing.T) {
	s, fc, _ := newTestServer(t)
	ctx := context.Background()
	if _, err := s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: "a1"}); err != nil {
		t.Fatal(err)
	}
	// Index entry lingers, but ateapi.GetActor reports the actor is gone.
	fc.actors = nil
	_, err := s.GetFleetActor(ctx, &atefleetpb.GetFleetActorRequest{ActorId: "a1"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound, got %v (%v)", status.Code(err), err)
	}
}

func actorIDs(as []*atefleetpb.FleetActor) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.GetActorId())
	}
	return out
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			return false
		}
		seen[w]--
	}
	return true
}

func TestTerminateActor(t *testing.T) {
	s, fc, _ := newTestServer(t)
	ctx := context.Background()
	s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: "a1"})
	if _, err := s.TerminateActor(ctx, &atefleetpb.TerminateActorRequest{ActorId: "a1"}); err != nil {
		t.Fatal(err)
	}
	if len(fc.deleted) != 1 || fc.deleted[0] != "a1" {
		t.Fatalf("deleted %v", fc.deleted)
	}
	// A dispatched actor is running; terminate must suspend it before delete.
	if len(fc.suspended) != 1 || fc.suspended[0] != "a1" {
		t.Fatalf("suspended %v (want [a1] before delete)", fc.suspended)
	}
	if _, err := s.idx.Get(ctx, "a1"); err != ErrNotFound {
		t.Fatal("index entry should be gone")
	}
}

func TestTerminateActorPreservesDownstreamCode(t *testing.T) {
	s, fc, _ := newTestServer(t)
	ctx := context.Background()
	s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: "a1"})
	// ateapi.DeleteActor returns FailedPrecondition for a running (not suspended) actor.
	fc.deleteErr = status.Error(codes.FailedPrecondition, "Actor a1 is not suspended")
	_, err := s.TerminateActor(ctx, &atefleetpb.TerminateActorRequest{ActorId: "a1"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v (%v)", status.Code(err), err)
	}
	// On a failed downstream delete the index entry must remain.
	if _, err := s.idx.Get(ctx, "a1"); err != nil {
		t.Fatalf("index entry should remain: %v", err)
	}
}

func TestRunSubtask(t *testing.T) {
	s, fc, fr := newTestServer(t)
	fr.res = RunResult{Stdout: "out\n", Stderr: "err\n", ExitCode: 3, RunError: "oops"}
	resp, err := s.RunSubtask(context.Background(), &atefleetpb.RunSubtaskRequest{
		ActorTemplateNamespace: "ns", ActorTemplateName: "t",
		Command: []string{"echo", "hi"}, TimeoutSeconds: 30, Owner: "alice", Group: "g1",
	})
	if err != nil {
		t.Fatalf("RunSubtask: %v", err)
	}

	id := resp.GetActorId()
	if !strings.HasPrefix(id, "subtask-") {
		t.Fatalf("actor id %q, want subtask- prefix", id)
	}
	if resp.GetStdout() != "out\n" || resp.GetStderr() != "err\n" || resp.GetExitCode() != 3 || resp.GetError() != "oops" {
		t.Fatalf("response = %+v, want canned runner result", resp)
	}

	// The runner saw the resumed actor's pod ip and the command.
	if fr.gotAddr != "10.0.0.1:80" {
		t.Fatalf("runner addr = %q, want 10.0.0.1:80", fr.gotAddr)
	}
	if len(fr.gotCommand) != 2 || fr.gotCommand[0] != "echo" || fr.gotCommand[1] != "hi" {
		t.Fatalf("runner command = %v, want [echo hi]", fr.gotCommand)
	}
	if fr.gotTimeout != 30*time.Second {
		t.Fatalf("runner timeout = %v, want 30s", fr.gotTimeout)
	}

	// Full one-shot lifecycle ran against the generated id.
	if !sameSet(fc.created, []string{id}) {
		t.Fatalf("created = %v, want [%s]", fc.created, id)
	}
	if !sameSet(fc.resumed, []string{id}) {
		t.Fatalf("resumed = %v, want [%s]", fc.resumed, id)
	}
	if !sameSet(fc.suspended, []string{id}) {
		t.Fatalf("suspended = %v, want [%s]", fc.suspended, id)
	}
	if !sameSet(fc.deleted, []string{id}) {
		t.Fatalf("deleted = %v, want [%s]", fc.deleted, id)
	}
	// Teardown also dropped the backstop index entry.
	if _, err := s.idx.Get(context.Background(), id); err != ErrNotFound {
		t.Fatalf("index entry should be gone after teardown, got err=%v", err)
	}
}

func TestRunSubtaskRunErrorStillTearsDown(t *testing.T) {
	s, fc, fr := newTestServer(t)
	fr.err = errors.New("dial failed")
	_, err := s.RunSubtask(context.Background(), &atefleetpb.RunSubtaskRequest{
		ActorTemplateNamespace: "ns", ActorTemplateName: "t", Command: []string{"echo"},
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("want Internal on runner error, got %v (%v)", status.Code(err), err)
	}
	// Teardown must still run via the defer even though Run failed.
	if len(fc.suspended) != 1 {
		t.Fatalf("suspended = %v, want one teardown call", fc.suspended)
	}
	if len(fc.deleted) != 1 {
		t.Fatalf("deleted = %v, want one teardown call", fc.deleted)
	}
}

func TestRunSubtaskInvalidArgs(t *testing.T) {
	s, _, _ := newTestServer(t)
	ctx := context.Background()
	// missing template
	if _, err := s.RunSubtask(ctx, &atefleetpb.RunSubtaskRequest{ActorTemplateName: "t", Command: []string{"echo"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing namespace: want InvalidArgument, got %v (%v)", status.Code(err), err)
	}
	// empty command
	if _, err := s.RunSubtask(ctx, &atefleetpb.RunSubtaskRequest{ActorTemplateNamespace: "ns", ActorTemplateName: "t"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty command: want InvalidArgument, got %v (%v)", status.Code(err), err)
	}
}

func scoped(owner string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(OwnerMetadataKey, owner))
}

func TestScope_DispatchStampsOwnerAndListFilters(t *testing.T) {
	s, _, _ := newTestServer(t)
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
	s, fc, _ := newTestServer(t)
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
