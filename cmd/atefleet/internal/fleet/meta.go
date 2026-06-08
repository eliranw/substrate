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

// List returns all fleet metadata entries. It uses KEYS for the MVP, which is
// fine at the expected fleet scale; switching to SCAN is a Phase 3 follow-up.
func (i *Index) List(ctx context.Context) ([]FleetMeta, error) {
	keys, err := i.rdb.Keys(ctx, indexPrefix+"*").Result()
	if err != nil {
		return nil, fmt.Errorf("redis keys: %w", err)
	}
	out := make([]FleetMeta, 0, len(keys))
	for _, k := range keys {
		b, err := i.rdb.Get(ctx, k).Bytes()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("redis get %q: %w", k, err)
		}
		var m FleetMeta
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("unmarshal %q: %w", k, err)
		}
		out = append(out, m)
	}
	return out, nil
}

func (i *Index) Delete(ctx context.Context, id string) error {
	if err := i.rdb.Del(ctx, key(id)).Err(); err != nil {
		return fmt.Errorf("redis del %q: %w", key(id), err)
	}
	return nil
}
