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
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	atefleetpb "github.com/agent-substrate/substrate/internal/proto/atefleetpb"
	ateapipb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// subtaskBackstopTTL bounds how long a one-shot subtask actor may linger if
// RunSubtask crashes before its teardown runs: the reaper deletes any subtask
// actor whose expiry has passed, so a crashed run leaks nothing.
const subtaskBackstopTTL = 600

type Server struct {
	atefleetpb.UnimplementedFleetManagerServer
	api    ControlAPI
	idx    *Index
	now    func() int64 // unix seconds; injectable for tests
	runner SubtaskRunner
}

func NewServer(api ControlAPI, idx *Index, now func() int64, runner SubtaskRunner) *Server {
	return &Server{api: api, idx: idx, now: now, runner: runner}
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

// RunSubtask runs a one-shot command on an ephemeral actor: create + resume a
// fresh actor, POST the command to its sandbox, then always tear the actor down
// (suspend + delete) regardless of the command's outcome.
func (s *Server) RunSubtask(ctx context.Context, r *atefleetpb.RunSubtaskRequest) (*atefleetpb.RunSubtaskResponse, error) {
	if r.GetActorTemplateNamespace() == "" || r.GetActorTemplateName() == "" {
		return nil, status.Error(codes.InvalidArgument, "actor_template_namespace and actor_template_name are required")
	}
	if len(r.GetCommand()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "command is required")
	}

	id := newSubtaskID()

	// Index a backstop entry first so a crash anywhere below still lets the
	// reaper clean the actor up once the backstop TTL elapses.
	meta := FleetMeta{
		ActorID: id, Role: "subtask", Owner: r.GetOwner(), Group: r.GetGroup(),
		ExpiryUnix:        s.now() + subtaskBackstopTTL,
		TemplateNamespace: r.GetActorTemplateNamespace(), TemplateName: r.GetActorTemplateName(),
	}
	if err := s.idx.Put(ctx, meta); err != nil {
		return nil, status.Errorf(codes.Internal, "index subtask: %v", err)
	}

	if _, err := s.api.CreateActor(ctx, &ateapipb.CreateActorRequest{
		ActorId: id, ActorTemplateNamespace: r.GetActorTemplateNamespace(), ActorTemplateName: r.GetActorTemplateName(),
	}); err != nil {
		// On a failed create there is no actor to tear down; just drop the index.
		if derr := s.idx.Delete(ctx, id); derr != nil {
			slog.WarnContext(ctx, "subtask: drop index after create failure", "actor", id, "err", derr)
		}
		if st, ok := status.FromError(err); ok {
			return nil, status.Error(st.Code(), st.Message())
		}
		return nil, status.Errorf(codes.Internal, "create actor: %v", err)
	}

	// From here the actor exists, so teardown must always run. The defer
	// suspends + deletes the actor and drops the index entry; teardown errors
	// are logged but never mask the primary result.
	defer func() {
		if _, err := s.api.SuspendActor(ctx, &ateapipb.SuspendActorRequest{ActorId: id}); err != nil {
			slog.WarnContext(ctx, "subtask teardown: suspend failed", "actor", id, "err", err)
		}
		if _, err := s.api.DeleteActor(ctx, &ateapipb.DeleteActorRequest{ActorId: id}); err != nil {
			slog.WarnContext(ctx, "subtask teardown: delete failed", "actor", id, "err", err)
		}
		if err := s.idx.Delete(ctx, id); err != nil {
			slog.WarnContext(ctx, "subtask teardown: drop index failed", "actor", id, "err", err)
		}
	}()

	if _, err := s.api.ResumeActor(ctx, &ateapipb.ResumeActorRequest{ActorId: id}); err != nil {
		if st, ok := status.FromError(err); ok {
			return nil, status.Error(st.Code(), st.Message())
		}
		return nil, status.Errorf(codes.Internal, "resume actor: %v", err)
	}

	ga, err := s.api.GetActor(ctx, &ateapipb.GetActorRequest{ActorId: id})
	if err != nil {
		if st, ok := status.FromError(err); ok {
			return nil, status.Error(st.Code(), st.Message())
		}
		return nil, status.Errorf(codes.Internal, "get actor: %v", err)
	}
	podIP := ga.GetActor().GetAteomPodIp()
	if podIP == "" {
		return nil, status.Error(codes.Internal, "actor has no pod ip")
	}
	addr := podIP + ":80"

	var timeout time.Duration
	if r.GetTimeoutSeconds() > 0 {
		timeout = time.Duration(r.GetTimeoutSeconds()) * time.Second
	}

	res, err := s.runner.Run(ctx, addr, r.GetCommand(), timeout)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "run subtask: %v", err)
	}

	return &atefleetpb.RunSubtaskResponse{
		Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: int32(res.ExitCode), Error: res.RunError, ActorId: id,
	}, nil
}

// newSubtaskID returns a fresh actor id for a one-shot subtask. The result is a
// valid DNS-1123 label ("subtask-" + 16 lowercase hex chars).
func newSubtaskID() string {
	var b [8]byte
	// crypto/rand.Read never returns an error on the platforms we target, but
	// even a (theoretical) zero id is a valid, unique-enough label here.
	_, _ = rand.Read(b[:])
	return "subtask-" + hex.EncodeToString(b[:])
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
