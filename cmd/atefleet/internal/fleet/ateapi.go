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

func (a *controlAdapter) CreateActor(ctx context.Context, in *ateapipb.CreateActorRequest) (*ateapipb.CreateActorResponse, error) {
	return a.c.CreateActor(ctx, in)
}
func (a *controlAdapter) ResumeActor(ctx context.Context, in *ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error) {
	return a.c.ResumeActor(ctx, in)
}
func (a *controlAdapter) GetActor(ctx context.Context, in *ateapipb.GetActorRequest) (*ateapipb.GetActorResponse, error) {
	return a.c.GetActor(ctx, in)
}
func (a *controlAdapter) ListActors(ctx context.Context, in *ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
	return a.c.ListActors(ctx, in)
}
func (a *controlAdapter) DeleteActor(ctx context.Context, in *ateapipb.DeleteActorRequest) (*ateapipb.DeleteActorResponse, error) {
	return a.c.DeleteActor(ctx, in)
}
