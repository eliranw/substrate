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
	"fmt"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ateapipb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

type Reaper struct {
	api ControlAPI
	idx *Index
	now func() int64
}

func NewReaper(api ControlAPI, idx *Index, now func() int64) *Reaper {
	return &Reaper{api: api, idx: idx, now: now}
}

// ReapOnce: delete expired actors; drop index entries whose actor no longer exists.
func (r *Reaper) ReapOnce(ctx context.Context) error {
	metas, err := r.idx.List(ctx)
	if err != nil {
		return fmt.Errorf("list index: %w", err)
	}
	// Page through the full actor list; reading only the first page would treat
	// alive actors on later pages as gone and destroy their index metadata.
	actors, err := listAllActors(ctx, r.api)
	if err != nil {
		return fmt.Errorf("list actors: %w", err)
	}
	live := map[string]bool{}
	for _, a := range actors {
		live[a.GetActorId()] = true
	}

	now := r.now()
	for _, m := range metas {
		switch {
		case !live[m.ActorID]:
			// Actor appears gone, but the full list could still race a recent
			// create. Confirm via GetActor before deleting: only reconcile the
			// stale index entry when ateapi reports the actor is truly absent.
			if _, err := r.api.GetActor(ctx, &ateapipb.GetActorRequest{ActorId: m.ActorID}); err != nil {
				if st, ok := status.FromError(err); !ok || st.Code() != codes.NotFound {
					// Ambiguous (transient) error — keep the index entry and
					// retry next tick rather than risk deleting live metadata.
					slog.WarnContext(ctx, "reaper: GetActor inconclusive; keeping index entry", "actor", m.ActorID, "err", err)
					continue
				}
			} else {
				// Actor actually exists; ListActors just didn't surface it. Keep it.
				continue
			}
			if err := r.idx.Delete(ctx, m.ActorID); err != nil {
				slog.WarnContext(ctx, "reaper: drop stale index entry failed", "actor", m.ActorID, "err", err)
				continue // one bad key must not stall the rest of the pass
			}
			slog.InfoContext(ctx, "reaper: dropped stale index entry", "actor", m.ActorID)
		case m.ExpiryUnix > 0 && now >= m.ExpiryUnix:
			// Expired actors are running; ateapi.DeleteActor only deletes suspended
			// actors, so suspend first (best-effort — delete surfaces any failure).
			if _, err := r.api.SuspendActor(ctx, &ateapipb.SuspendActorRequest{ActorId: m.ActorID}); err != nil {
				slog.WarnContext(ctx, "reaper: suspend expired actor failed (continuing to delete)", "actor", m.ActorID, "err", err)
			}
			if _, err := r.api.DeleteActor(ctx, &ateapipb.DeleteActorRequest{ActorId: m.ActorID}); err != nil {
				slog.WarnContext(ctx, "reaper: delete expired actor failed", "actor", m.ActorID, "err", err)
				continue // keep index; retry next tick
			}
			if err := r.idx.Delete(ctx, m.ActorID); err != nil {
				slog.WarnContext(ctx, "reaper: delete expired index entry failed", "actor", m.ActorID, "err", err)
				continue // one bad key must not stall the rest of the pass
			}
			slog.InfoContext(ctx, "reaper: reaped expired actor", "actor", m.ActorID)
		}
	}
	return nil
}
