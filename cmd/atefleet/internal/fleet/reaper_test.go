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

	atefleetpb "github.com/agent-substrate/substrate/internal/proto/atefleetpb"
)

func TestReapOnce(t *testing.T) {
	s, fc := newTestServer(t) // now() == 1000
	ctx := context.Background()
	// expired: ttl makes expiry 1000+1=1001 but we'll reap at now=2000
	s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: "old", TtlSeconds: 1})
	// live, no ttl
	s.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{ActorTemplateNamespace: "ns", ActorTemplateName: "t", ActorId: "keep"})
	// stale index entry (actor not in ateapi)
	s.idx.Put(ctx, FleetMeta{ActorID: "ghost"})

	r := NewReaper(fc, s.idx, func() int64 { return 2000 })
	if err := r.ReapOnce(ctx); err != nil {
		t.Fatal(err)
	}

	if len(fc.deleted) != 1 || fc.deleted[0] != "old" {
		t.Fatalf("deleted %v (want [old])", fc.deleted)
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
}
