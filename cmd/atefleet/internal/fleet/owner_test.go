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
