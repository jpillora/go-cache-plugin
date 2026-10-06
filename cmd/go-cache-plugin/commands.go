// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/creachadair/command"
	"github.com/creachadair/gocache"
	"github.com/creachadair/taskgroup"
	"github.com/jpillora/go-cache-plugin/lib/clientcache"
	"github.com/jpillora/go-cache-plugin/lib/remotecache"
)

var flags struct {
	CacheDir      string        `flag:"cache-dir,default=$GOCACHE_DIR,Local cache directory (required)"`
	S3Bucket      string        `flag:"bucket,default=$GOCACHE_S3_BUCKET,S3 bucket name (required)"`
	S3Region      string        `flag:"region,default=$GOCACHE_S3_REGION,S3 region"`
	S3Endpoint    string        `flag:"s3-endpoint-url,default=$GOCACHE_S3_ENDPOINT_URL,S3 custom endpoint URL (if unset, use AWS default)"`
	S3PathStyle   bool          `flag:"s3-path-style,default=$GOCACHE_S3_PATH_STYLE,S3 path-style URLs (optional)"`
	KeyPrefix     string        `flag:"prefix,default=$GOCACHE_KEY_PREFIX,S3 key prefix (optional)"`
	MinUploadSize int64         `flag:"min-upload-size,default=$GOCACHE_MIN_SIZE,Minimum object size to upload to S3 (in bytes)"`
	Concurrency   int           `flag:"c,default=$GOCACHE_CONCURRENCY,Maximum number of concurrent requests"`
	S3Concurrency int           `flag:"u,default=$GOCACHE_S3_CONCURRENCY,Maximum concurrency for upload to S3"`
	PrintMetrics  bool          `flag:"metrics,default=$GOCACHE_METRICS,Print summary metrics to stderr at exit"`
	Expiration    time.Duration `flag:"expiry,default=$GOCACHE_EXPIRY,Cache expiration period (optional)"`
	Verbose       bool          `flag:"v,default=$GOCACHE_VERBOSE,Enable verbose logging"`
	DebugLog      int           `flag:"debug,default=$GOCACHE_DEBUG,Enable detailed per-request debug logging (noisy)"`
}

const (
	debugBuildCache = 1 << iota
	debugModProxy
	debugRevProxy
)

// runDirect runs a cache communicating on stdin/stdout, for use as a direct
// GOCACHEPROG plugin.
func runDirect(env *command.Env) error {
	s, _, err := initCacheServer(env)
	if err != nil {
		return err
	}
	if err := s.Run(env.Context(), os.Stdin, os.Stdout); err != nil {
		return fmt.Errorf("cache server exited with error: %w", err)
	}
	if flags.Verbose || flags.PrintMetrics {
		fmt.Fprintln(os.Stderr, s.Metrics())
	}
	return nil
}

var serveFlags struct {
	Plugin   string `flag:"plugin,default=$GOCACHE_PLUGIN,Plugin service address (port or [host]:port; required)"`
	HTTP     string `flag:"http,default=$GOCACHE_HTTP,HTTP service address ([host]:port)"`
	ModProxy bool   `flag:"modproxy,default=$GOCACHE_MODPROXY,Enable a Go module proxy (requires --http)"`
	RevProxy string `flag:"revproxy,default=$GOCACHE_REVPROXY,Reverse proxy these hosts (comma-separated; requires --http)"`
	SumDB    string `flag:"sumdb,default=$GOCACHE_SUMDB,SumDB servers to proxy for (comma-separated)"`
}

var connectFlags struct {
	CacheSize string `flag:"cache-size,default=$GOCACHE_CLIENT_MAX_SIZE,Client LFU cache limit (default 2GiB; 0 keeps only build working files)"`
}

func noopClose(context.Context) error { return nil }

// runServe runs a cache communicating over a local TCP socket.
func runServe(env *command.Env) error {
	addr, err := pluginAddress(serveFlags.Plugin)
	if err != nil {
		return env.Usagef("invalid --plugin address: %v", err)
	}

	// Initialize the cache server. Unlike a direct server, only close down and
	// wait for cache cleanup when the whole process exits.
	s, s3c, err := initCacheServer(env)
	if err != nil {
		return err
	}
	closeHook := s.Close
	s.Close = noopClose

	// Listen for connections from the Go toolchain on the specified socket.
	lst, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	log.Printf("plugin listening at %q", lst.Addr())

	ctx, cancel := signal.NotifyContext(env.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var g taskgroup.Group
	g.Run(func() {
		<-ctx.Done()
		log.Printf("closing plugin listener")
		lst.Close()
	})

	// If a module proxy is enabled, start it.
	modProxy, modCleanup, err := initModProxy(env.SetContext(ctx), s3c)
	if err != nil {
		lst.Close()
		return fmt.Errorf("module proxy: %w", err)
	}
	defer modCleanup()

	// If a reverse proxy is enabled, start it.
	revProxy, err := initRevProxy(env.SetContext(ctx), s3c, &g)
	if err != nil {
		lst.Close()
		return fmt.Errorf("reverse proxy: %w", err)
	}

	// If an HTTP server is enabled, start it up with debug routes
	// and whatever other services were requested.
	if serveFlags.HTTP != "" {
		srv := &http.Server{
			Addr:    serveFlags.HTTP,
			Handler: makeHandler(remotecache.NewHandler(s), modProxy, revProxy),
		}
		g.Go(srv.ListenAndServe)
		vprintf("HTTP server listening at %q", serveFlags.HTTP)
		g.Run(func() {
			<-ctx.Done()
			vprintf("stopping HTTP service")
			srv.Shutdown(context.Background())
		})
	}

	for {
		conn, err := lst.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("accept failed: %v, exiting server loop", err)
			}
			break
		}
		log.Printf("new client connection")
		g.Go(func() error {
			defer func() {
				log.Printf("client connection closed")
				conn.Close()
			}()
			return s.Run(ctx, conn, conn)
		})
	}
	log.Printf("server loop exited, waiting for client exit")
	g.Wait()
	if closeHook != nil {
		ctx := gocache.WithLogf(context.Background(), log.Printf)
		if err := closeHook(ctx); err != nil {
			log.Printf("server close: %v (ignored)", err)
		}
	}
	return nil
}

// runConnect implements a direct cache proxy by connecting to a remote server.
func runConnect(env *command.Env, plugin string) error {
	if strings.HasPrefix(plugin, "http://") || strings.HasPrefix(plugin, "https://") {
		return runHTTPConnect(env, plugin)
	}
	port, err := strconv.Atoi(plugin)
	if err != nil || port <= 0 || port > 65535 {
		return env.Usagef("provide a local port or an http(s):// server URL")
	}

	conn, err := net.Dial("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	start := time.Now()
	vprintf("connected to %q", conn.RemoteAddr())

	out := taskgroup.Go(func() error {
		defer conn.(*net.TCPConn).CloseWrite() // let the server finish
		return copy(conn, os.Stdin)
	})
	if rerr := copy(os.Stdout, conn); rerr != nil {
		vprintf("read responses: %v", err)
	}
	out.Wait()
	conn.Close()
	vprintf("connection closed (%v elapsed)", time.Since(start))
	return nil
}

// pluginAddress preserves localhost as the default for a bare port, while
// allowing an explicit host to expose the listener on other interfaces.
func pluginAddress(address string) (string, error) {
	if !strings.Contains(address, ":") {
		address = net.JoinHostPort("127.0.0.1", address)
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", err
	}
	number, err := strconv.Atoi(port)
	if err != nil || number <= 0 || number > 65535 {
		return "", fmt.Errorf("port must be between 1 and 65535")
	}
	return address, nil
}

// runHTTPConnect stages remote artifacts locally before returning paths to Go.
// Unlike direct and serve modes, this mode needs no S3 configuration.
func runHTTPConnect(env *command.Env, serverURL string) error {
	cacheDir := flags.CacheDir
	if cacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("find client cache directory: %w", err)
		}
		cacheDir = filepath.Join(base, "go-cache-plugin-client")
	}
	limit := connectFlags.CacheSize
	if limit == "" {
		limit = "2GiB"
	}
	maxBytes, err := clientcache.ParseLimit(limit)
	if err != nil {
		return env.Usagef("invalid --cache-size: %v", err)
	}
	client, err := remotecache.NewClientWithLimit(serverURL, cacheDir, maxBytes)
	if err != nil {
		return err
	}
	defer client.Close()
	client.Logf = vprintf
	s := &gocache.Server{
		Get:         client.Get,
		Put:         client.Put,
		Close:       func(context.Context) error { return client.Close() },
		MaxRequests: flags.Concurrency,
		Logf:        vprintf,
		LogRequests: flags.DebugLog&debugBuildCache != 0,
	}
	return s.Run(env.Context(), os.Stdin, os.Stdout)
}

// copy emulates the base case of io.Copy, but does not attempt to use the
// io.ReaderFrom or io.WriterTo implementations.
//
// TODO(creachadair): For some reason io.Copy does not work correctly when r is
// a pipe (e.g., stdin) and w is a TCP socket. Figure out why.
func copy(w io.Writer, r io.Reader) error {
	var buf [4096]byte
	for {
		nr, err := r.Read(buf[:])
		if nr > 0 {
			if nw, err := w.Write(buf[:nr]); err != nil {
				return fmt.Errorf("copy to: %w", err)
			} else if nw < nr {
				return fmt.Errorf("wrote %d < %d bytes: %w", nw, nr, io.ErrShortWrite)
			}
		}
		if err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("copy from: %w", err)
		}
	}
}
