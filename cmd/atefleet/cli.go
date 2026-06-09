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
	"crypto/tls"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	atefleetpb "github.com/agent-substrate/substrate/internal/proto/atefleetpb"
)

// fleetAddr is the persistent --fleet-addr flag value shared by the client
// subcommands.
var fleetAddr string

// newRootCmd builds the atefleet cobra root command. It owns the persistent
// --fleet-addr flag used by the client subcommands to dial the FleetManager.
func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "atefleet",
		Short:        "Manage a fleet of Agent Substrate actors via the FleetManager",
		SilenceUsage: true,
	}
	cmd.PersistentFlags().StringVar(&fleetAddr, "fleet-addr", "atefleet.ate-system.svc:443", "Address of the atefleet FleetManager gRPC service.")
	return cmd
}

// dialFleet dials the FleetManager service. It mirrors serve.go's ateapi dial
// (TLS with InsecureSkipVerify) and returns the client plus a closer for the
// underlying connection.
func dialFleet() (atefleetpb.FleetManagerClient, func(), error) {
	conn, err := grpc.NewClient(fleetAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("dial atefleet %q: %w", fleetAddr, err)
	}
	return atefleetpb.NewFleetManagerClient(conn), func() { _ = conn.Close() }, nil
}

func newDispatchCmd() *cobra.Command {
	var template, id, role, owner, group string
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "dispatch",
		Short: "Dispatch a new actor into the fleet",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			parts := strings.Split(template, "/")
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return fmt.Errorf("malformed --template %q (expected <namespace>/<name>)", template)
			}
			client, closeFn, err := dialFleet()
			if err != nil {
				return err
			}
			defer closeFn()

			resp, err := client.DispatchActor(ctx, &atefleetpb.DispatchActorRequest{
				ActorTemplateNamespace: parts[0],
				ActorTemplateName:      parts[1],
				ActorId:                id,
				Role:                   role,
				Owner:                  owner,
				Group:                  group,
				TtlSeconds:             int64(ttl.Seconds()),
			})
			if err != nil {
				return err
			}
			return printFleetActor(resp.GetActor())
		},
	}
	cmd.Flags().StringVarP(&template, "template", "t", "", "Actor template in <namespace>/<name> format (required)")
	cmd.Flags().StringVar(&id, "id", "", "Actor id (DNS-1123 label, required)")
	cmd.Flags().StringVar(&role, "role", "", "Role label to assign to the actor")
	cmd.Flags().StringVar(&owner, "owner", "", "Owner label to assign to the actor")
	cmd.Flags().StringVar(&group, "group", "", "Group label to assign to the actor")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "Time-to-live before the reaper terminates the actor (0 = no expiry)")
	_ = cmd.MarkFlagRequired("template")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func newLsCmd() *cobra.Command {
	var role, owner, group string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List actors in the fleet",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			client, closeFn, err := dialFleet()
			if err != nil {
				return err
			}
			defer closeFn()

			resp, err := client.ListFleet(ctx, &atefleetpb.ListFleetRequest{
				Role:  role,
				Owner: owner,
				Group: group,
			})
			if err != nil {
				return err
			}
			return printFleetTable(resp.GetActors())
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "Filter by role")
	cmd.Flags().StringVar(&owner, "owner", "", "Filter by owner")
	cmd.Flags().StringVar(&group, "group", "", "Filter by group")
	return cmd
}

func newGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <actor-id>",
		Short: "Get a single actor from the fleet",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, closeFn, err := dialFleet()
			if err != nil {
				return err
			}
			defer closeFn()

			resp, err := client.GetFleetActor(ctx, &atefleetpb.GetFleetActorRequest{ActorId: args[0]})
			if err != nil {
				return err
			}
			return printFleetActor(resp.GetActor())
		},
	}
	return cmd
}

func newRmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <actor-id>",
		Short: "Terminate an actor and remove it from the fleet",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, closeFn, err := dialFleet()
			if err != nil {
				return err
			}
			defer closeFn()

			if _, err := client.TerminateActor(ctx, &atefleetpb.TerminateActorRequest{ActorId: args[0]}); err != nil {
				return err
			}
			fmt.Printf("actor %q terminated\n", args[0])
			return nil
		},
	}
	return cmd
}

func newRunCmd() *cobra.Command {
	var template, owner, group string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "run --template <ns>/<name> [flags] -- <cmd> [args...]",
		Short: "Run a one-shot command in an ephemeral actor",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			parts := strings.Split(template, "/")
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return fmt.Errorf("malformed --template %q (expected <namespace>/<name>)", template)
			}
			client, closeFn, err := dialFleet()
			if err != nil {
				return err
			}
			defer closeFn()

			resp, err := client.RunSubtask(ctx, &atefleetpb.RunSubtaskRequest{
				ActorTemplateNamespace: parts[0],
				ActorTemplateName:      parts[1],
				Command:                args,
				TimeoutSeconds:         int64(timeout.Seconds()),
				Owner:                  owner,
				Group:                  group,
			})
			if err != nil {
				return err
			}

			// Mirror the real command's semantics: relay stdout/stderr and exit
			// with the subtask's exit code.
			fmt.Fprint(os.Stdout, resp.GetStdout())
			fmt.Fprint(os.Stderr, resp.GetStderr())
			if resp.GetError() != "" {
				fmt.Fprintln(os.Stderr, resp.GetError())
			}
			os.Exit(int(resp.GetExitCode()))
			return nil
		},
	}
	cmd.Flags().StringVarP(&template, "template", "t", "", "Actor template in <namespace>/<name> format (required)")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "Time limit for the command (0 = runner default)")
	cmd.Flags().StringVar(&owner, "owner", "", "Owner label to assign to the ephemeral actor")
	cmd.Flags().StringVar(&group, "group", "", "Group label to assign to the ephemeral actor")
	_ = cmd.MarkFlagRequired("template")
	return cmd
}

// printFleetActor prints a single FleetActor as a one-row table.
func printFleetActor(a *atefleetpb.FleetActor) error {
	return printFleetTable([]*atefleetpb.FleetActor{a})
}

// printFleetTable prints fleet actors as a tab-aligned table, mirroring the
// kubectl-ate table style.
func printFleetTable(actors []*atefleetpb.FleetActor) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tROLE\tOWNER\tGROUP\tADDRESS")
	for _, a := range actors {
		if a == nil {
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			orNone(a.GetActorId()),
			orNone(a.GetStatus()),
			orNone(a.GetRole()),
			orNone(a.GetOwner()),
			orNone(a.GetGroup()),
			orNone(a.GetAddress()),
		)
	}
	return w.Flush()
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}
