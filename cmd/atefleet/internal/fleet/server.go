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
		return nil, status.Errorf(codes.Internal, "create actor: %v", err)
	}
	resumeResp, err := s.api.ResumeActor(ctx, &ateapipb.ResumeActorRequest{ActorId: r.GetActorId()})
	if err != nil {
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

func fleetActor(m FleetMeta, addr string, st ateapipb.Actor_Status) *atefleetpb.FleetActor {
	return &atefleetpb.FleetActor{
		ActorId: m.ActorID, Address: addr, Status: st.String(),
		Role: m.Role, Owner: m.Owner, Group: m.Group, ExpiryUnix: m.ExpiryUnix,
	}
}
