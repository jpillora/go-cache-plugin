// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/creachadair/gocache"
	"github.com/jpillora/go-cache-plugin/lib/clientcache"
	"github.com/jpillora/go-cache-plugin/lib/remotecache"
)

func TestBoundedServerSessions(t *testing.T) {
	directory := t.TempDir()
	root, err := clientcache.New(directory, 56<<10)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	local := &boundedLocal{root: root}
	server := &cacheServer{Server: &gocache.Server{Get: local.Get, Put: local.Put}, local: local}
	first, release, err := server.session(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	data := bytes.Repeat([]byte("a"), 8<<10)
	action := fmt.Sprintf("%x", sha256.Sum256([]byte("first")))
	output := fmt.Sprintf("%x", sha256.Sum256(data))
	pinned, err := local.Put(first, gocache.Object{ActionID: action, OutputID: output, Size: int64(len(data)), Body: bytes.NewReader(data)})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.handler(remotecache.NewHandler(server.Server)))
	defer httpServer.Close()
	for _, name := range []string{"b", "c", "d"} {
		data := bytes.Repeat([]byte(name), 8<<10)
		id := fmt.Sprintf("%x", sha256.Sum256(data))
		req, err := http.NewRequest(http.MethodPut, httpServer.URL+"/cache/"+id+"/"+id, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatal(resp.Status)
		}
		// Each HTTP request releases its pins; only the TCP-style session remains.
		builds, err := os.ReadDir(filepath.Join(directory, "builds"))
		if err != nil || len(builds) != 2 {
			t.Fatalf("leaked request pins: %d, %v", len(builds), err)
		}
		resp, err = http.Get(httpServer.URL + "/cache/" + id)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("HTTP cache response: %v", err)
		}
	}
	sizeData, err := os.ReadFile(filepath.Join(directory, ".size"))
	if err != nil {
		t.Fatal(err)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(sizeData)), 10, 64)
	if err != nil || size > 56<<10 {
		t.Fatalf("cache size %d exceeded limit: %v", size, err)
	}
	got, err := os.ReadFile(pinned)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("eviction invalidated active build: %v", err)
	}
	release()
	if _, err := os.Stat(pinned); !os.IsNotExist(err) {
		t.Fatalf("build pin not released: %v", err)
	}
}
