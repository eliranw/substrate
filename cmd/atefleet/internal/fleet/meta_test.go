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

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestIndex(t *testing.T) *Index {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return NewIndex(rdb)
}

func TestIndexPutGetListDelete(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)
	m := FleetMeta{ActorID: "a1", Role: "worker", Owner: "eliran", Group: "g1", ExpiryUnix: 123, TemplateNamespace: "ns", TemplateName: "tmpl"}
	if err := idx.Put(ctx, m); err != nil {
		t.Fatal(err)
	}

	got, err := idx.Get(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Owner != "eliran" || got.ExpiryUnix != 123 {
		t.Fatalf("got %+v", got)
	}

	all, err := idx.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ActorID != "a1" {
		t.Fatalf("list = %+v", all)
	}

	if err := idx.Delete(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Get(ctx, "a1"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
