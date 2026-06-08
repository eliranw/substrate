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
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	if r.GetActorTemplateNamespace() == "" || r.GetActorTemplateName() == "" {
		return nil, status.Error(codes.InvalidArgument, "actor_template_namespace and actor_template_name are required")
	}

	if _, err := s.api.CreateActor(ctx, &ateapipb.CreateActorRequest{
		ActorId: r.GetActorId(), ActorTemplateNamespace: r.GetActorTemplateNamespace(), ActorTemplateName: r.GetActorTemplateName(),
	}); err != nil {
		// Preserve the downstream gRPC code (AlreadyExists, FailedPrecondition, ...)
		// so callers see the real reason instead of an opaque Internal.
		if st, ok := status.FromError(err); ok {
			return nil, status.Error(st.Code(), st.Message())
		}
		return nil, status.Errorf(codes.Internal, "create actor: %v", err)
	}
	resumeResp, err := s.api.ResumeActor(ctx, &ateapipb.ResumeActorRequest{ActorId: r.GetActorId()})
	if err != nil {
		// Preserve the downstream gRPC code (NotFound, Aborted, ...).
		if st, ok := status.FromError(err); ok {
			return nil, status.Error(st.Code(), st.Message())
		}
		return nil, status.Errorf(codes.Internal, "resume actor: %v", err)
	}

	var expiry int64
	if r.GetTtlSeconds() > 0 {
		expiry = s.now() + r.GetTtlSeconds()
	}
	meta := FleetMeta{
		ActorID: r.GetActorId(), Role: r.GetRole(), Owner: r.GetOwner(), Group: r.GetGroup(),
		ExpiryUnix: expiry, TemplateNamespace: r.GetActorTemplateNamespace(), TemplateName: r.GetActorTemplateName(),
	}
	if err := s.idx.Put(ctx, meta); err != nil {
		return nil, status.Errorf(codes.Internal, "index actor: %v", err)
	}

	return &atefleetpb.DispatchActorResponse{Actor: fleetActor(meta, addr, resumeResp.GetActor().GetStatus())}, nil
}

func (s *Server) ListFleet(ctx context.Context, r *atefleetpb.ListFleetRequest) (*atefleetpb.ListFleetResponse, error) {
	metas, err := s.idx.List(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list index: %v", err)
	}
	actors, err := listAllActors(ctx, s.api)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list actors: %v", err)
	}
	stByID := map[string]ateapipb.Actor_Status{}
	for _, a := range actors {
		stByID[a.GetActorId()] = a.GetStatus()
	}

	out := &atefleetpb.ListFleetResponse{}
	for _, m := range metas {
		if r.GetRole() != "" && m.Role != r.GetRole() {
			continue
		}
		if r.GetOwner() != "" && m.Owner != r.GetOwner() {
			continue
		}
		if r.GetGroup() != "" && m.Group != r.GetGroup() {
			continue
		}
		st, live := stByID[m.ActorID]
		if !live {
			continue // actor gone but index lingering — skip (reaper cleans it)
		}
		addr, _ := ActorAddress(m.ActorID)
		out.Actors = append(out.Actors, fleetActor(m, addr, st))
	}
	return out, nil
}

func (s *Server) GetFleetActor(ctx context.Context, r *atefleetpb.GetFleetActorRequest) (*atefleetpb.GetFleetActorResponse, error) {
	m, err := s.idx.Get(ctx, r.GetActorId())
	if err == ErrNotFound {
		return nil, status.Error(codes.NotFound, "actor not in fleet")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get index: %v", err)
	}
	ga, err := s.api.GetActor(ctx, &ateapipb.GetActorRequest{ActorId: r.GetActorId()})
	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
			// actor gone but index lingering — same race ListFleet skips (reaper cleans it).
			return nil, status.Error(codes.NotFound, "actor not in fleet")
		}
		return nil, status.Errorf(codes.Internal, "get actor: %v", err)
	}
	addr, _ := ActorAddress(m.ActorID)
	return &atefleetpb.GetFleetActorResponse{Actor: fleetActor(m, addr, ga.GetActor().GetStatus())}, nil
}

func (s *Server) TerminateActor(ctx context.Context, r *atefleetpb.TerminateActorRequest) (*atefleetpb.TerminateActorResponse, error) {
	// ateapi.DeleteActor only deletes suspended actors; MVP surfaces that error.
	// Preserve the downstream gRPC code (NotFound, FailedPrecondition, InvalidArgument,
	// Aborted) so callers see the real reason instead of an opaque Internal.
	if _, err := s.api.DeleteActor(ctx, &ateapipb.DeleteActorRequest{ActorId: r.GetActorId()}); err != nil {
		if st, ok := status.FromError(err); ok {
			return nil, status.Error(st.Code(), st.Message())
		}
		return nil, status.Errorf(codes.Internal, "delete actor: %v", err)
	}
	if err := s.idx.Delete(ctx, r.GetActorId()); err != nil {
		return nil, status.Errorf(codes.Internal, "delete index: %v", err)
	}
	return &atefleetpb.TerminateActorResponse{}, nil
}

// listAllActors pages through ateapi.ListActors fully, accumulating every
// actor across all pages. Reading only the first page (as the MVP did)
// under-reports the fleet and, in the reaper, would destroy index metadata for
// alive actors that only appear on later pages.
func listAllActors(ctx context.Context, api ControlAPI) ([]*ateapipb.Actor, error) {
	var out []*ateapipb.Actor
	tok := ""
	for {
		resp, err := api.ListActors(ctx, &ateapipb.ListActorsRequest{PageToken: tok})
		if err != nil {
			return nil, err
		}
		out = append(out, resp.GetActors()...)
		tok = resp.GetNextPageToken()
		if tok == "" {
			break
		}
	}
	return out, nil
}

func fleetActor(m FleetMeta, addr string, st ateapipb.Actor_Status) *atefleetpb.FleetActor {
	return &atefleetpb.FleetActor{
		ActorId: m.ActorID, Address: addr, Status: st.String(),
		Role: m.Role, Owner: m.Owner, Group: m.Group, ExpiryUnix: m.ExpiryUnix,
	}
}
