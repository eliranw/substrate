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
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/agent-substrate/substrate/cmd/atefleet/internal/fleet"
	"github.com/agent-substrate/substrate/internal/credbundle"
	atefleetpb "github.com/agent-substrate/substrate/internal/proto/atefleetpb"
	ateapipb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

func newServeCmd() *cobra.Command {
	var listen, ateapiAddr, redisAddr, serverCredBundle string
	var redisCACerts, redisTLSServerName, redisClientCert string
	var reapEvery time.Duration
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the atefleet FleetManager gRPC service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			// Resolve the `@env` sentinel for the redis flags against the
			// matching environment variables, mirroring cmd/ateapi/main.go's
			// loadFlagsFromEnv. This lets the deployment share the
			// ate-api-server-envvars ConfigMap without relying on fragile
			// Kubernetes $(VAR) arg expansion.
			for _, o := range []struct {
				flag *string
				env  string
			}{
				{&redisAddr, "ATE_API_REDIS_ADDRESS"},
				{&redisTLSServerName, "ATE_API_REDIS_TLS_SERVER_NAME"},
				{&redisClientCert, "ATE_API_REDIS_CLIENT_CERT"},
			} {
				if *o.flag == "@env" {
					*o.flag = os.Getenv(o.env)
				}
			}

			// Dial the ateapi Control service. This mirrors
			// internal/ateclient.dialDirect (TLS with InsecureSkipVerify +
			// otel stats handler).
			// TODO(live-gate): confirm ateapi client identity (podcert mTLS /
			// client-JWT) against a live ateapi.
			conn, err := grpc.NewClient(ateapiAddr,
				grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})),
				grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
			)
			if err != nil {
				return fmt.Errorf("dial ateapi %q: %w", ateapiAddr, err)
			}
			api := fleet.NewControlAPI(ateapipb.NewControlClient(conn))

			// Connect to the Redis/Valkey cluster. The TLS config mirrors
			// cmd/ateapi/main.go's buildRedisTLSConfig.
			// TODO(live-gate): mirror ateapi connectRedis IAM auth
			// (CredentialsProvider) against a live Valkey cluster.
			tlsConfig, err := buildRedisTLSConfig(ctx, redisCACerts, redisTLSServerName, redisClientCert)
			if err != nil {
				return err
			}
			rdb := redis.NewClusterClient(&redis.ClusterOptions{
				Addrs:     []string{redisAddr},
				TLSConfig: tlsConfig,
			})
			if err := rdb.Ping(ctx).Err(); err != nil {
				return fmt.Errorf("ping redis %q: %w", redisAddr, err)
			}
			idx := fleet.NewIndex(rdb)
			nowUnix := func() int64 { return time.Now().Unix() }

			srv := fleet.NewServer(api, idx, nowUnix, fleet.NewHTTPRunner(nil))
			reaper := fleet.NewReaper(api, idx, nowUnix)
			go runReaper(ctx, reaper, reapEvery)

			lis, err := net.Listen("tcp", listen)
			if err != nil {
				return fmt.Errorf("listen on %q: %w", listen, err)
			}
			// Serve TLS using the servicedns pod-certificate credential bundle,
			// mirroring cmd/ateapi/main.go's buildServerCreds. The FleetManager
			// clients (kubectl-ate / atefleet subcommands) dial over TLS with
			// InsecureSkipVerify, so the server must terminate TLS.
			if serverCredBundle == "" {
				return fmt.Errorf("--grpc-server-cred-bundle is required")
			}
			serverCreds := credentials.NewTLS(&tls.Config{
				GetCertificate: credbundle.Loader(serverCredBundle),
			})
			g := grpc.NewServer(
				grpc.Creds(serverCreds),
				grpc.StatsHandler(otelgrpc.NewServerHandler()),
			)
			atefleetpb.RegisterFleetManagerServer(g, srv)
			slog.InfoContext(ctx, "atefleet serving", "addr", listen)
			return g.Serve(lis)
		},
	}
	cmd.Flags().StringVar(&listen, "grpc-listen-addr", "0.0.0.0:443", "Address and port the gRPC server should listen on.")
	cmd.Flags().StringVar(&ateapiAddr, "ateapi-addr", "api.ate-system.svc:443", "Address of the ateapi Control gRPC service.")
	cmd.Flags().StringVar(&serverCredBundle, "grpc-server-cred-bundle", "", "File with the server TLS credential bundle.")
	cmd.Flags().StringVar(&redisAddr, "redis-cluster-address", "", "The address of the redis/valkey cluster.")
	cmd.Flags().StringVar(&redisCACerts, "redis-ca-certs", "", "The file that contains the CA certificate for the Redis cluster.")
	cmd.Flags().StringVar(&redisTLSServerName, "redis-tls-server-name", "", "The ServerName to use for Redis TLS hostname verification.")
	cmd.Flags().StringVar(&redisClientCert, "redis-client-cert", "", "The file containing the client TLS certificate/key credential bundle for Redis/Valkey.")
	cmd.Flags().DurationVar(&reapEvery, "reap-interval", 30*time.Second, "How often the TTL/stale reaper runs.")
	return cmd
}

// buildRedisTLSConfig builds the Redis/Valkey TLS config. It mirrors
// cmd/ateapi/main.go's buildRedisTLSConfig: optional custom CA pool, custom
// ServerName for hostname verification, and a client credential bundle.
func buildRedisTLSConfig(ctx context.Context, caCerts, serverName, clientCert string) (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if caCerts != "" {
		ca, err := os.ReadFile(caCerts)
		if err != nil {
			return nil, fmt.Errorf("read Redis CA cert: %w", err)
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("parse Redis CA cert from %s", caCerts)
		}
		tlsConfig.RootCAs = caPool
		slog.InfoContext(ctx, "Using custom CA cert for Redis", slog.String("path", caCerts))
	}
	if serverName != "" {
		tlsConfig.ServerName = serverName
		slog.InfoContext(ctx, "Using custom ServerName for Redis TLS verification", slog.String("name", serverName))
	}
	if clientCert != "" {
		cert, err := credbundle.Parse(clientCert)
		if err != nil {
			return nil, fmt.Errorf("parse Redis client credential bundle: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{*cert}
		slog.InfoContext(ctx, "Using client TLS certificate for Redis/Valkey", slog.String("path", clientCert))
	}
	return tlsConfig, nil
}

func runReaper(ctx context.Context, r *fleet.Reaper, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.ReapOnce(ctx); err != nil {
				slog.WarnContext(ctx, "reaper tick failed", "err", err)
			}
		}
	}
}
