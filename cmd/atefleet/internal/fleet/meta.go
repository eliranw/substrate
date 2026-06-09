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
	"encoding/json"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
)

var ErrNotFound = errors.New("fleet: not found")

const indexPrefix = "atefleet:actor:"

// indexSetKey is a single Redis key holding the set of all fleet actor ids.
// List reads this set (a single key is slot-consistent on a Redis Cluster)
// instead of KEYS/SCAN, which only scan one shard on a cluster and would
// silently under-report the fleet.
const indexSetKey = "atefleet:actor-ids"

// FleetMeta is atefleet's per-Actor metadata, kept in its own Redis index.
// ateapi.ListActors remains the source of truth for existence/state.
type FleetMeta struct {
	ActorID           string `json:"actor_id"`
	Role              string `json:"role,omitempty"`
	Owner             string `json:"owner,omitempty"`
	Group             string `json:"group,omitempty"`
	ExpiryUnix        int64  `json:"expiry_unix,omitempty"`
	TemplateNamespace string `json:"template_namespace,omitempty"`
	TemplateName      string `json:"template_name,omitempty"`
}

type Index struct{ rdb redis.Cmdable }

func NewIndex(rdb redis.Cmdable) *Index { return &Index{rdb: rdb} }

func key(id string) string { return indexPrefix + id }

func (i *Index) Put(ctx context.Context, m FleetMeta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal fleet meta: %w", err)
	}
	if err := i.rdb.Set(ctx, key(m.ActorID), b, 0).Err(); err != nil {
		return fmt.Errorf("redis set %q: %w", key(m.ActorID), err)
	}
	// Track the id in the index set so List is correct on a Redis Cluster.
	if err := i.rdb.SAdd(ctx, indexSetKey, m.ActorID).Err(); err != nil {
		return fmt.Errorf("redis sadd %q: %w", indexSetKey, err)
	}
	return nil
}

func (i *Index) Get(ctx context.Context, id string) (FleetMeta, error) {
	b, err := i.rdb.Get(ctx, key(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return FleetMeta{}, ErrNotFound
	}
	if err != nil {
		return FleetMeta{}, fmt.Errorf("redis get %q: %w", key(id), err)
	}
	var m FleetMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return FleetMeta{}, fmt.Errorf("unmarshal: %w", err)
	}
	return m, nil
}

// List returns all fleet metadata entries. It reads the actor-id index set
// (a single key, so it is consistent on a Redis Cluster) and fetches each entry
// by key. This avoids KEYS/SCAN, which only scan a single shard on a cluster
// and would under-report the fleet.
func (i *Index) List(ctx context.Context) ([]FleetMeta, error) {
	ids, err := i.rdb.SMembers(ctx, indexSetKey).Result()
	if err != nil {
		return nil, fmt.Errorf("redis smembers %q: %w", indexSetKey, err)
	}
	out := make([]FleetMeta, 0, len(ids))
	for _, id := range ids {
		b, err := i.rdb.Get(ctx, key(id)).Bytes()
		if errors.Is(err, redis.Nil) {
			// Meta gone but id lingering in the set — self-heal and skip.
			i.rdb.SRem(ctx, indexSetKey, id)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("redis get %q: %w", key(id), err)
		}
		var m FleetMeta
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("unmarshal %q: %w", key(id), err)
		}
		out = append(out, m)
	}
	return out, nil
}

func (i *Index) Delete(ctx context.Context, id string) error {
	if err := i.rdb.Del(ctx, key(id)).Err(); err != nil {
		return fmt.Errorf("redis del %q: %w", key(id), err)
	}
	if err := i.rdb.SRem(ctx, indexSetKey, id).Err(); err != nil {
		return fmt.Errorf("redis srem %q: %w", indexSetKey, err)
	}
	return nil
}
