// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Program gocache implements the experimental GOCACHEPROG protocol over an S3
// bucket, for use in builder and CI workers.
package main

import (
	"context"
	"log"
	"os"

	"github.com/creachadair/command"
	"github.com/creachadair/flax"
	"github.com/jpillora/go-cache-plugin/lib/s3util"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	root := &command.C{
		Name:  command.ProgramName(),
		Usage: "--cache-dir d --bucket b [options]\nhelp",
		Help: `Run a cache service for the Go toolchain backed by an S3 bucket.

This program serves the Go toolchain cache protocol on stdin/stdout.
It is meant to be run by the "go" tool as a GOCACHEPROG plugin.
For example:

    GOCACHEPROG=` + command.ProgramName() + ` go build ./...

Note that Go toolchains prior to 1.24 must be built with GOEXPERIMENT=cacheprog
to enable this integration. Go 1.24 and later have it enabled by default.

You must provide --cache-dir, --bucket, and --region, or the corresponding
environment variables (see "help environment").  Entries in the cache are
stored in the specified S3 bucket, and staged in a local directory specified by
the --cache-dir flag or GOCACHE_DIR environment.`,

		SetFlags: command.Flags(flax.MustBind, &flags),
		Run:      command.Adapt(runDirect),

		Commands: []*command.C{
			{
				Name:  "serve",
				Usage: "--plugin <port|host:port>",
				Help: `Run a cache server.

In this mode, the cache server listens for connections on a socket instead of
serving directly over stdin/stdout. The "connect" command adapts the direct
interface to this one.

The --plugin address accepts a port (localhost) or an explicit host:port.
Use --plugin=0.0.0.0:5930 to listen on all IPv4 interfaces.

If --http is set, the server also exports an HTTP server at that address.
This exports /debug endpoints, including metrics, and an HTTP build-cache API
under /cache/. Remote clients use "connect http://<host>:<port>" to download
artifacts locally; no shared filesystem or S3 credentials are needed on clients.
When --http is enabled, the following options are available:

- When --modproxy is true, the server also exports a caching module proxy at
  http://<host>:<port>/mod/.

- When --revproxy is set, the server also hosts a caching reverse proxy for the
  specified hosts at http://<host>:<port>. The reverse proxy handles both HTTP
  and HTTPS requests, and caches immutable successful responses.`,

				SetFlags: command.Flags(flax.MustBind, &serveFlags),
				Run:      command.Adapt(runServe),
			},
			{
				Name:  "connect",
				Usage: "<port|http(s)://server>",
				Help: `Connect to a remote cache server.

For a local port, this bridges stdin/stdout to the server's TCP listener.
For an HTTP or HTTPS URL, this downloads artifacts to a client-local cache before
returning file paths to Go. This requires no shared filesystem or S3 credentials.
The client cache defaults to the OS cache directory under go-cache-plugin-client;
override it with --cache-dir or GOCACHE_DIR. It retains at most 2GiB using LFU
eviction, configurable with --cache-size or GOCACHE_CLIENT_MAX_SIZE. Active builds
pin their working files until they close; those files may temporarily exceed the
persistent cache limit. Use --cache-size=0 to retain no persistent local cache.`,

				SetFlags: command.Flags(flax.MustBind, &connectFlags),
				Run:      command.Adapt(runConnect),
			},
			command.HelpCommand(helpTopics),
			command.VersionCommand(),
		},
	}
	command.RunOrFail(root.NewEnv(nil), os.Args[1:])
}

// getBucketRegion reports the specified region for the given bucket.
// if the --region flag was set, that value is returned without error.
// Otherwise, it queries the GetBucketLocation API.
func getBucketRegion(ctx context.Context, bucket string) (string, error) {
	if flags.S3Region != "" {
		return flags.S3Region, nil
	}
	return s3util.BucketRegion(ctx, bucket)
}

// vprintf acts as log.Printf if the --verbose flag is set; otherwise it
// discards its input.
func vprintf(msg string, args ...any) {
	if flags.Verbose || flags.DebugLog != 0 {
		log.Printf(msg, args...)
	}
}
