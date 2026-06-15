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

package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/agent-substrate/substrate/cmd/atefleet/internal/axconfig"
)

func newCtxCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "ctx", Short: "Manage atefleet contexts (kubeconfig-style)"}
	cmd.AddCommand(newCtxListCmd(), newCtxUseCmd(), newCtxSetCmd())
	cmd.RunE = func(c *cobra.Command, _ []string) error { return runCtxList() } // bare `ctx` = list
	return cmd
}

func newCtxListCmd() *cobra.Command {
	return &cobra.Command{Use: "list", Short: "List contexts", RunE: func(_ *cobra.Command, _ []string) error { return runCtxList() }}
}

func runCtxList() error {
	cfg, err := axconfig.Load()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "CURRENT\tNAME\tFLEET-ADDR\tOWNER")
	for _, c := range cfg.Contexts {
		mark := ""
		if c.Name == cfg.CurrentContext {
			mark = "*"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", mark, c.Name, c.FleetAddr, c.Owner)
	}
	return w.Flush()
}

func newCtxUseCmd() *cobra.Command {
	return &cobra.Command{
		Use: "use <name>", Short: "Set the current context", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := axconfig.Load()
			if err != nil {
				return err
			}
			if err := cfg.Use(args[0]); err != nil {
				return err
			}
			fmt.Printf("switched to context %q\n", args[0])
			return nil
		},
	}
}

func newCtxSetCmd() *cobra.Command {
	var addr, owner string
	cmd := &cobra.Command{
		Use: "set <name>", Short: "Add or update a context", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := axconfig.Load()
			if err != nil {
				return err
			}
			if err := cfg.Set(axconfig.Context{Name: args[0], FleetAddr: addr, Owner: owner}); err != nil {
				return err
			}
			fmt.Printf("context %q set\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "fleet-addr", "", "FleetManager address for this context")
	cmd.Flags().StringVar(&owner, "owner", "", "Owner asserted by this context")
	return cmd
}
