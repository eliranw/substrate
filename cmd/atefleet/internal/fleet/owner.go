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

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// OwnerMetadataKey is the gRPC metadata header carrying the caller's asserted
// owner (trusted-claim scoping). Shared with the CLI client.
const OwnerMetadataKey = "x-atefleet-owner"

// ownerFromCtx returns the asserted owner from incoming gRPC metadata ("" = none).
func ownerFromCtx(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	if v := md.Get(OwnerMetadataKey); len(v) > 0 {
		return v[0]
	}
	return ""
}

// OwnerUnaryInterceptor gates calls when requireOwner is set: a call with no
// asserted owner is rejected. Otherwise it is a no-op (handlers read the owner
// via ownerFromCtx).
func OwnerUnaryInterceptor(requireOwner bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if requireOwner && ownerFromCtx(ctx) == "" {
			return nil, status.Errorf(codes.Unauthenticated, "missing %s metadata", OwnerMetadataKey)
		}
		return handler(ctx, req)
	}
}
