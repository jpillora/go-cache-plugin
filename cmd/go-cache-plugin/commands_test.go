// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/creachadair/gocache"
	"github.com/creachadair/gocache/cachedir"
	"github.com/jpillora/go-cache-plugin/lib/remotecache"
)

func TestPluginAddress(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"5930", "127.0.0.1:5930"},
		{"0.0.0.0:5930", "0.0.0.0:5930"},
		{":5930", ":5930"},
		{"localhost:5930", "localhost:5930"},
		{"[::1]:5930", "[::1]:5930"},
		{"", ""}, {"0", ""}, {"-1", ""}, {"65536", ""}, {"host:bad", ""},
	} {
		got, err := pluginAddress(test.input)
		if test.want == "" {
			if err == nil {
				t.Errorf("pluginAddress(%q) accepted invalid input", test.input)
			}
		} else if err != nil || got != test.want {
			t.Errorf("pluginAddress(%q) = %q, %v; want %q", test.input, got, err, test.want)
		}
	}
}

// The test executable doubles as a real GOCACHEPROG subprocess. os.Exit keeps
// the testing harness from writing its PASS line into the protocol stream.
func TestCachePluginProcess(t *testing.T) {
	if os.Getenv("GO_CACHE_PLUGIN_TEST_PROCESS") != "1" {
		return
	}
	separator := slices.Index(os.Args, "--")
	if separator < 0 {
		os.Exit(2)
	}
	os.Args = append([]string{"go-cache-plugin"}, os.Args[separator+1:]...)
	main()
	os.Exit(0)
}

func TestRemoteGoBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the Go toolchain through the remote client")
	}
	store, err := cachedir.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int64
	cache := &gocache.Server{
		Get: func(ctx context.Context, action string) (string, string, error) {
			output, path, err := store.Get(ctx, action)
			if output != "" {
				hits.Add(1)
			}
			return output, path, err
		},
		Put: store.Put,
	}
	server := httptest.NewServer(makeHandler(remotecache.NewHandler(cache), nil, nil))
	defer server.Close()
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte("module example.com/remote-cache-test\n\ngo 1.26.1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "main.go"), []byte("package main\nfunc main() { println(\"remote cache OK\") }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		clientDir := t.TempDir()
		pluginCommand := strconv.Quote(os.Args[0]) + " -test.run=^TestCachePluginProcess$ -- connect " + strconv.Quote("--cache-dir="+clientDir) + " " + server.URL
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		build := exec.CommandContext(ctx, "go", "build", "-o", "app", ".")
		build.Dir = project
		build.Env = append(os.Environ(),
			"GOCACHEPROG="+pluginCommand,
			"GO_CACHE_PLUGIN_TEST_PROCESS=1",
			"GOCACHE="+t.TempDir(),
			"GOCACHE_DIR=",
			"GOCACHE_S3_BUCKET=",
			"AWS_SHARED_CREDENTIALS_FILE="+filepath.Join(t.TempDir(), "missing"),
			"AWS_EC2_METADATA_DISABLED=true",
		)
		before := hits.Load()
		result, err := build.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("client %d build: %v\n%s", i+1, err, result)
		}
		if i == 1 && hits.Load() <= before {
			t.Fatal("second client did not get any remote cache hits")
		}
		entries, err := os.ReadDir(filepath.Join(clientDir, "output"))
		if err != nil || len(entries) == 0 {
			t.Fatalf("client %d did not stage its own artifacts: %v", i+1, err)
		}
		result, err = exec.CommandContext(t.Context(), filepath.Join(project, "app")).CombinedOutput()
		if err != nil || strings.TrimSpace(string(result)) != "remote cache OK" {
			t.Fatalf("client %d executable: %v, %q", i+1, err, result)
		}
	}
}
