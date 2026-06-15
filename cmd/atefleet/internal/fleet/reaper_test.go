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
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	atefleetpb "github.com/agent-substrate/substrate/internal/proto/atefleetpb"
)

func TestReapOnce(t *testing.T) {
	s, fc, _ := newTestServer(t) // now() == 1000
	ctx := context.Background()
	// expired: ttl makes expiry 1000+1=1001 but we'll reap at now=2000
	s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: "old", TtlSeconds: 1})
	// not-yet-expired: expiry 1000+5000=6000 is in the future relative to reap-now=2000
	s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: "future", TtlSeconds: 5000})
	// live, no ttl
	s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: "keep"})
	// stale index entry (actor not in ateapi). GetActor must report NotFound.
	s.idx.Put(ctx, FleetMeta{ActorID: "ghost"})

	r := NewReaper(fc, s.idx, func() int64 { return 2000 })
	if err := r.ReapOnce(ctx); err != nil {
		t.Fatal(err)
	}

	if len(fc.deleted) != 1 || fc.deleted[0] != "old" {
		t.Fatalf("deleted %v (want [old])", fc.deleted)
	}
	// The expired actor is running; the reaper must suspend it before delete.
	if len(fc.suspended) != 1 || fc.suspended[0] != "old" {
		t.Fatalf("suspended %v (want [old] before delete)", fc.suspended)
	}
	if _, err := s.idx.Get(ctx, "old"); err != ErrNotFound {
		t.Fatal("old index not cleared")
	}
	if _, err := s.idx.Get(ctx, "ghost"); err != ErrNotFound {
		t.Fatal("ghost index not reconciled")
	}
	if _, err := s.idx.Get(ctx, "keep"); err != nil {
		t.Fatal("keep should remain")
	}
	if _, err := s.idx.Get(ctx, "future"); err != nil {
		t.Fatal("future (not-yet-expired) should remain")
	}
}

// TestReapOnceBoundaryExpiry: now == expiry reaps (>= boundary).
func TestReapOnceBoundaryExpiry(t *testing.T) {
	s, fc, _ := newTestServer(t) // now() == 1000
	ctx := context.Background()
	// expiry 1000+1000 = 2000; reap at now == 2000 must reap.
	s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: "edge", TtlSeconds: 1000})

	r := NewReaper(fc, s.idx, func() int64 { return 2000 })
	if err := r.ReapOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fc.deleted) != 1 || fc.deleted[0] != "edge" {
		t.Fatalf("deleted %v (want [edge])", fc.deleted)
	}
	if _, err := s.idx.Get(ctx, "edge"); err != ErrNotFound {
		t.Fatal("edge index not cleared at now==expiry boundary")
	}
}

// TestReapOnceDeleteFailureKeepsIndexAndContinues: a DeleteActor failure on an
// expired actor must keep its index entry and not stall the rest of the pass.
func TestReapOnceDeleteFailureKeepsIndexAndContinues(t *testing.T) {
	s, fc, _ := newTestServer(t) // now() == 1000
	ctx := context.Background()
	s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: "exp1", TtlSeconds: 1})
	s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: "exp2", TtlSeconds: 1})
	// Stale ghost too: ensures the pass keeps reconciling after a delete failure.
	s.idx.Put(ctx, FleetMeta{ActorID: "ghost"})

	// DeleteActor always fails with FailedPrecondition.
	fc.deleteErr = status.Error(codes.FailedPrecondition, "Actor is not suspended")

	r := NewReaper(fc, s.idx, func() int64 { return 2000 })
	if err := r.ReapOnce(ctx); err != nil {
		t.Fatalf("pass should not abort on a single delete failure: %v", err)
	}

	// Both expired actors still have their index entries (delete failed).
	if _, err := s.idx.Get(ctx, "exp1"); err != nil {
		t.Fatalf("exp1 index should remain after delete failure: %v", err)
	}
	if _, err := s.idx.Get(ctx, "exp2"); err != nil {
		t.Fatalf("exp2 index should remain after delete failure: %v", err)
	}
	// The ghost (truly gone) was still reconciled despite the delete failures.
	if _, err := s.idx.Get(ctx, "ghost"); err != ErrNotFound {
		t.Fatal("ghost index not reconciled after delete failure on another entry")
	}
}

// TestReapOnceMultiPageDoesNotReapAlive: an alive actor that only appears on
// page 2 of ListActors must NOT have its index reaped (data-loss regression).
func TestReapOnceMultiPageDoesNotReapAlive(t *testing.T) {
	s, fc, _ := newTestServer(t) // now() == 1000
	ctx := context.Background()
	for _, id := range []string{"p1", "p2", "p3", "p4", "p5"} {
		s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: id})
	}
	// Force pagination: 2 actors per page so p3/p4/p5 only show on later pages.
	fc.pageSize = 2

	r := NewReaper(fc, s.idx, func() int64 { return 2000 })
	if err := r.ReapOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fc.deleted) != 0 {
		t.Fatalf("no actor should be deleted, got %v", fc.deleted)
	}
	for _, id := range []string{"p1", "p2", "p3", "p4", "p5"} {
		if _, err := s.idx.Get(ctx, id); err != nil {
			t.Fatalf("%s index should remain (paginated alive actor): %v", id, err)
		}
	}
}

// TestReapOnceKeepsIndexWhenGetActorAmbiguous: if an actor is missing from the
// (full) live set but GetActor returns a non-NotFound error, the index entry
// must be KEPT (defense in depth against transient ateapi errors).
func TestReapOnceKeepsIndexWhenGetActorAmbiguous(t *testing.T) {
	s, fc, _ := newTestServer(t) // now() == 1000
	ctx := context.Background()
	// Index entry with no corresponding actor in ListActors.
	s.idx.Put(ctx, FleetMeta{ActorID: "maybe"})
	// GetActor returns a transient error (not NotFound).
	fc.getErr = status.Error(codes.Unavailable, "ateapi temporarily down")

	r := NewReaper(fc, s.idx, func() int64 { return 2000 })
	if err := r.ReapOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.idx.Get(ctx, "maybe"); err != nil {
		t.Fatalf("index must be kept when GetActor is ambiguous: %v", err)
	}
}
