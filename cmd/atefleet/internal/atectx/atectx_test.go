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

package atectx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePrecedence(t *testing.T) {
	cfg := &Config{CurrentContext: "a", Contexts: []Context{{Name: "a", FleetAddr: "ctx-addr", Owner: "ctx-owner"}}}

	// flag wins
	addr, owner := cfg.Resolve("flag-addr", "flag-owner")
	if addr != "flag-addr" || owner != "flag-owner" {
		t.Fatalf("flag precedence: got %q %q", addr, owner)
	}
	// env beats context
	t.Setenv("ATEFLEET_FLEET_ADDR", "env-addr")
	t.Setenv("ATEFLEET_OWNER", "env-owner")
	addr, owner = cfg.Resolve("", "")
	if addr != "env-addr" || owner != "env-owner" {
		t.Fatalf("env precedence: got %q %q", addr, owner)
	}
	// context beats default when no flag/env
	os.Unsetenv("ATEFLEET_FLEET_ADDR")
	os.Unsetenv("ATEFLEET_OWNER")
	addr, owner = cfg.Resolve("", "")
	if addr != "ctx-addr" || owner != "ctx-owner" {
		t.Fatalf("ctx precedence: got %q %q", addr, owner)
	}
	// default when nothing set
	empty := &Config{}
	addr, owner = empty.Resolve("", "")
	if addr != DefaultFleetAddr || owner != "" {
		t.Fatalf("default: got %q %q", addr, owner)
	}
}

func TestLoadSaveUseSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("ATEFLEET_CONFIG", path)

	// missing file -> empty config, no error
	c, err := Load()
	if err != nil || len(c.Contexts) != 0 {
		t.Fatalf("load missing: %+v %v", c, err)
	}
	// set adds a context and (first one) becomes current; persists
	if err := c.Set(Context{Name: "alice", FleetAddr: "localhost:18443", Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	c2, _ := Load()
	if c2.CurrentContext != "alice" || len(c2.Contexts) != 1 || c2.Contexts[0].Owner != "alice" {
		t.Fatalf("after set: %+v", c2)
	}
	// use unknown -> error
	if err := c2.Use("nope"); err == nil {
		t.Fatal("expected error using unknown context")
	}
	// Active resolves the current context
	a, err := c2.Active()
	if err != nil || a.Owner != "alice" {
		t.Fatalf("active: %+v %v", a, err)
	}
}
