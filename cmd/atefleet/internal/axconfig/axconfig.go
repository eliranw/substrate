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

// Package axconfig is atefleet's kubeconfig-style context config: named
// contexts each asserting a fleet-addr and owner, with flag>env>context>default
// resolution.
package axconfig

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// DefaultFleetAddr is used when no flag/env/context supplies one.
const DefaultFleetAddr = "atefleet.ate-system.svc:443"

type Context struct {
	Name      string `json:"name"`
	FleetAddr string `json:"fleet-addr"`
	Owner     string `json:"owner,omitempty"`
}

type Config struct {
	CurrentContext string    `json:"current-context"`
	Contexts       []Context `json:"contexts"`
}

// path is $ATEFLEET_CONFIG or ~/.atefleet/config.yaml.
func path() string {
	if p := os.Getenv("ATEFLEET_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".atefleet", "config.yaml")
}

// Load reads the config; a missing file yields an empty config (not an error).
func Load() (*Config, error) {
	b, err := os.ReadFile(path())
	if os.IsNotExist(err) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path(), err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path(), err)
	}
	return &c, nil
}

func (c *Config) save() error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path(), b, 0o600)
}

// Active returns the current-context, or an error if unset/unknown.
func (c *Config) Active() (Context, error) {
	if c.CurrentContext == "" {
		return Context{}, fmt.Errorf("no current-context set")
	}
	for _, x := range c.Contexts {
		if x.Name == c.CurrentContext {
			return x, nil
		}
	}
	return Context{}, fmt.Errorf("current-context %q not found", c.CurrentContext)
}

// Use sets the current-context (must exist) and persists.
func (c *Config) Use(name string) error {
	for _, x := range c.Contexts {
		if x.Name == name {
			c.CurrentContext = name
			return c.save()
		}
	}
	return fmt.Errorf("context %q not found", name)
}

// Set upserts a context (by name) and persists. The first context added also
// becomes current-context for convenience.
func (c *Config) Set(ctx Context) error {
	for i, x := range c.Contexts {
		if x.Name == ctx.Name {
			c.Contexts[i] = ctx
			return c.save()
		}
	}
	c.Contexts = append(c.Contexts, ctx)
	if c.CurrentContext == "" {
		c.CurrentContext = ctx.Name
	}
	return c.save()
}

// Resolve applies precedence: flag > env > active context > default.
func (c *Config) Resolve(flagAddr, flagOwner string) (addr, owner string) {
	var act Context
	if a, err := c.Active(); err == nil {
		act = a
	}
	addr = first(flagAddr, os.Getenv("ATEFLEET_FLEET_ADDR"), act.FleetAddr, DefaultFleetAddr)
	owner = first(flagOwner, os.Getenv("ATEFLEET_OWNER"), act.Owner)
	return
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
