# go-cache-plugin

[![GoDoc](https://img.shields.io/static/v1?label=godoc&message=reference&color=lightgrey)](https://pkg.go.dev/github.com/jpillora/go-cache-plugin)
[![CI](https://github.com/jpillora/go-cache-plugin/actions/workflows/go-presubmit.yml/badge.svg?event=push&branch=main)](https://github.com/jpillora/go-cache-plugin/actions/workflows/go-presubmit.yml)

This fork of
[tailscale/go-cache-plugin](https://github.com/tailscale/go-cache-plugin)
implements a `GOCACHEPROG` plugin backed by Amazon S3 or compatible services
such as Cloudflare R2. It adds remote HTTP clients that download build artifacts
into their own local cache, and configurable TCP bind addresses.

## Installation

```shell
GOTOOLCHAIN=go1.26.5 go install github.com/jpillora/go-cache-plugin/cmd/go-cache-plugin@latest
```

## Usage Outline

```shell
export GOCACHEPROG="go-cache-plugin --cache-dir=/tmp/gocache --bucket=some-s3-bucket"
go test ./...
```

Go 1.24 and later support `GOCACHEPROG` by default. Earlier toolchains require
`GOEXPERIMENT=cacheprog`. Build this binary with Go 1.26.x; the current upstream
dependencies are incompatible with Go 1.27.

## Discussion

The `go-cache-plugin` program supports two modes of operation:

1. **Direct mode**: The program is invoked directly by the Go toolchain as a
   subprocess, and exits when the toolchain execution ends.

   This is the default mode of operation, and requires no additional setup.

2. **Server mode**: The program runs as a separate process and the Go toolchain
   communicates with it over a local socket.

   This mode requires the server to be started up ahead of time, but makes the
   configuration for the toolchain simpler. This mode also permits running an
   in-process module and sum database proxy.

### Server Mode

To run in server mode, use the `serve` subcommand:

```sh
# N.B.: The --plugin flag is required.
go-cache-plugin serve \
   --plugin=5930 \
   --cache-dir=/tmp/gocache \
   --bucket=some-s3-bucket
```

To connect to a server running in this mode, use the `connect` subcommand:

```sh
# Use the same port given to the server's --plugin flag.
# Mnemonic: 5930 == (Go) (C)ache (P)lugin
export GOCACHEPROG="go-cache-plugin connect 5930"
go build ./...
```

The `connect` command just bridges the socket to stdin/stdout, which is how the
Go toolchain expects to talk to the plugin.

### Remote Clients

Enable HTTP on the server, using its existing S3/R2 configuration:

```sh
go-cache-plugin serve \
   --plugin=0.0.0.0:5930 \
   --http=0.0.0.0:5970 \
   --cache-dir=/var/cache/go-cache-plugin \
   --bucket=some-s3-bucket --region=some-region
```

On each client, install this fork and point `GOCACHEPROG` at the HTTP endpoint:

```sh
GOTOOLCHAIN=go1.26.5 go install github.com/jpillora/go-cache-plugin/cmd/go-cache-plugin@latest
export GOCACHEPROG="go-cache-plugin connect http://server:5970"
go build ./...
```

Replace `server` with the server's Tailscale IP or DNS name. Use a trusted
network such as Tailscale; the build-cache API does not add authentication.
HTTPS URLs are also supported when the service is behind a TLS reverse proxy.

Clients need no S3/R2 credentials, SSH tunnels, `socat`, or filesystem mounts.
The client transfers binary artifacts over HTTP, verifies their SHA-256 output
IDs, and returns paths in its own cache to Go. It checks that local cache first
on subsequent builds. Upload failures leave usable local entries and are logged
when `-v` is enabled; remote read failures are handled as cache lookup errors by
Go.

The client cache defaults to `go-cache-plugin-client` under the OS cache
directory (`~/.cache` on Linux and `~/Library/Caches` on macOS). Override it
with `--cache-dir` or `GOCACHE_DIR`:

```sh
export GOCACHEPROG="go-cache-plugin connect --cache-dir=$HOME/.cache/go-remote http://server:5970"
```

For local TCP clients, the original `connect 5930` interface is unchanged. A
bare `--plugin=5930` still binds to localhost; use an explicit address to change
it.

### Running a Module Proxy

To enable a caching module proxy, use the `--modproxy` flag to `serve`. The
module proxy uses HTTP, not the plugin interface, use `--http` to set the
address:

```sh
go-cache-plugin serve \
   --plugin=5930 \
   --http=localhost:5970 --modproxy \
   --cache-dir=/tmp/gocache \
   # ... other flags
```

To tell the Go toolchain about the proxy, set:

```sh
# Mnemonic: 5970 == (Go) (M)odule (P)roxy
export GOPROXY=http://localhost:5970/mod   # use the --http address
```

If you want to also proxy queries to `sum.golang.org`, also add:

```sh
export GOSUMDB='sum.golang.org http://locahost:5970/mod/sumdb/sum.golang.org'
```

## References

- [Cache plugin protocol (proposal)](https://github.com/golang/go/issues/59719)
- [Cache plugin library](https://github.com/creachadair/gocache)
- [Go module proxy documentation](https://proxy.golang.org)
