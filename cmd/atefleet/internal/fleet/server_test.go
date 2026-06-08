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
	for _, a := range f.actors {
		if a.ActorId == in.GetActorId() {
			a.Status = ateapipb.Actor_STATUS_RUNNING
			return &ateapipb.ResumeActorResponse{Actor: a}, nil
		}
	}
	return &ateapipb.ResumeActorResponse{}, nil
}
func (f *fakeControl) GetActor(_ context.Context, in *ateapipb.GetActorRequest) (*ateapipb.GetActorResponse, error) {
	for _, a := range f.actors {
		if a.ActorId == in.GetActorId() {
			return &ateapipb.GetActorResponse{Actor: a}, nil
		}
	}
	return nil, ErrNotFound
}
func (f *fakeControl) ListActors(_ context.Context, _ *ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
	return &ateapipb.ListActorsResponse{Actors: f.actors}, nil
}
func (f *fakeControl) DeleteActor(_ context.Context, in *ateapipb.DeleteActorRequest) (*ateapipb.DeleteActorResponse, error) {
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

func newTestServer(t *testing.T) (*Server, *fakeControl) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	idx := NewIndex(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	fc := &fakeControl{}
	return NewServer(fc, idx, func() int64 { return 1000 }), fc
}

func TestDispatchActor(t *testing.T) {
	s, fc := newTestServer(t)
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
