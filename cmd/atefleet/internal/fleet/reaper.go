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
	// TODO(phase3): ListActors is paginated; reads only the first page.
	la, err := r.api.ListActors(ctx, &ateapipb.ListActorsRequest{})
	if err != nil {
		return fmt.Errorf("list actors: %w", err)
	}
	live := map[string]bool{}
	for _, a := range la.GetActors() {
		live[a.GetActorId()] = true
	}

	now := r.now()
	for _, m := range metas {
		switch {
		case !live[m.ActorID]:
			// actor gone (deleted out-of-band) — reconcile the stale index entry
			if err := r.idx.Delete(ctx, m.ActorID); err != nil {
				return err
			}
			slog.InfoContext(ctx, "reaper: dropped stale index entry", "actor", m.ActorID)
		case m.ExpiryUnix > 0 && now >= m.ExpiryUnix:
			if _, err := r.api.DeleteActor(ctx, &ateapipb.DeleteActorRequest{ActorId: m.ActorID}); err != nil {
				slog.WarnContext(ctx, "reaper: delete expired actor failed", "actor", m.ActorID, "err", err)
				continue // keep index; retry next tick
			}
			if err := r.idx.Delete(ctx, m.ActorID); err != nil {
				return err
			}
			slog.InfoContext(ctx, "reaper: reaped expired actor", "actor", m.ActorID)
		}
	}
	return nil
}
