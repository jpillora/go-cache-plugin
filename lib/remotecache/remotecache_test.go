// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package remotecache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/creachadair/gocache"
	"github.com/creachadair/gocache/cachedir"
)

func hashID(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func newLocal(t *testing.T) *cachedir.Dir {
	t.Helper()
	dir, err := cachedir.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func newClient(t *testing.T, serverURL, directory string) *Client {
	t.Helper()
	client, err := NewClient(serverURL, directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestIndependentClients(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("binary\x00artifact\xff\n")} {
		t.Run(fmt.Sprintf("size=%d", len(data)), func(t *testing.T) {
			ctx := context.Background()
			store := newLocal(t)
			server := httptest.NewServer(NewHandler(&gocache.Server{Get: store.Get, Put: store.Put}))
			defer server.Close()
			firstDir, secondDir := t.TempDir(), t.TempDir()
			first := newClient(t, server.URL, firstDir)
			second := newClient(t, server.URL, secondDir)
			action, output := hashID([]byte(t.Name())), hashID(data)
			firstPath, err := first.Put(ctx, gocache.Object{
				ActionID: action, OutputID: output, Size: int64(len(data)), Body: bytes.NewReader(data),
			})
			if err != nil {
				t.Fatal(err)
			}
			gotOutput, secondPath, err := second.Get(ctx, action)
			if err != nil || gotOutput != output {
				t.Fatalf("Get = %q, %q, %v", gotOutput, secondPath, err)
			}
			_, serverPath, err := store.Get(ctx, action)
			if err != nil {
				t.Fatal(err)
			}
			if firstPath == secondPath || secondPath == serverPath || !strings.HasPrefix(secondPath, secondDir+string(filepath.Separator)) {
				t.Fatalf("paths were not staged independently: %q, %q, %q", firstPath, secondPath, serverPath)
			}
			got, err := os.ReadFile(secondPath)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("downloaded artifact = %q, %v; want %q", got, err, data)
			}
			server.Close()
			if gotOutput, gotPath, err := second.Get(ctx, action); err != nil || gotOutput != output || gotPath != secondPath {
				t.Fatalf("offline local hit = %q, %q, %v", gotOutput, gotPath, err)
			}
		})
	}
}

func TestMiss(t *testing.T) {
	store := newLocal(t)
	server := httptest.NewServer(NewHandler(&gocache.Server{Get: store.Get, Put: store.Put}))
	defer server.Close()
	client := newClient(t, server.URL, t.TempDir())
	output, path, err := client.Get(context.Background(), hashID([]byte("missing")))
	if err != nil || output != "" || path != "" {
		t.Fatalf("cache miss = %q, %q, %v", output, path, err)
	}
}

func TestFailedUploadKeepsLocalArtifact(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := newClient(t, server.URL, t.TempDir())
	var messages []string
	client.Logf = func(format string, args ...any) { messages = append(messages, fmt.Sprintf(format, args...)) }
	data := []byte("local fallback")
	action, output := hashID([]byte(t.Name())), hashID(data)
	path, err := client.Put(context.Background(), gocache.Object{
		ActionID: action, OutputID: output, Size: int64(len(data)), Body: bytes.NewReader(data),
	})
	if err != nil || path == "" || len(messages) != 1 {
		t.Fatalf("failed-upload fallback = %q, %v; logs %v", path, err, messages)
	}
	gotOutput, gotPath, err := client.Get(context.Background(), action)
	if err != nil || gotOutput != output || gotPath != path {
		t.Fatalf("local fallback Get = %q, %q, %v", gotOutput, gotPath, err)
	}
}

func TestInvalidDownloadsDoNotPopulateCache(t *testing.T) {
	for _, test := range []struct {
		name, output, body, length string
	}{
		{"missing ID", "", "ok", "2"},
		{"invalid ID", "../escape", "ok", "2"},
		{"wrong checksum", hashID([]byte("good")), "evil", "4"},
		{"truncated body", hashID([]byte("good")), "g", "4"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(outputIDHeader, test.output)
				w.Header().Set("Content-Length", test.length)
				io.WriteString(w, test.body)
			}))
			defer server.Close()
			client := newClient(t, server.URL, t.TempDir())
			action := hashID([]byte(t.Name()))
			if _, _, err := client.Get(context.Background(), action); err == nil {
				t.Fatal("invalid download was accepted")
			}
			if output, path, err := client.local.Get(context.Background(), action); err != nil || output != "" || path != "" {
				t.Fatalf("invalid artifact entered local cache: %q, %q, %v", output, path, err)
			}
		})
	}
}

func TestRejectInvalidRequests(t *testing.T) {
	store := newLocal(t)
	handler := NewHandler(&gocache.Server{Get: store.Get, Put: store.Put})
	action, output := hashID([]byte("action")), hashID([]byte("valid"))
	for _, test := range []struct {
		name, method, path, body string
		length                   int64
		status                   int
	}{
		{"short action", "GET", "/cache/a", "", 0, http.StatusBadRequest},
		{"uppercase action", "GET", "/cache/" + strings.ToUpper(action), "", 0, http.StatusBadRequest},
		{"bad output", "PUT", "/cache/" + action + "/invalid", "valid", 5, http.StatusBadRequest},
		{"missing length", "PUT", "/cache/" + action + "/" + output, "valid", -1, http.StatusLengthRequired},
		{"wrong checksum", "PUT", "/cache/" + action + "/" + output, "wrong", 5, http.StatusInternalServerError},
		{"wrong length", "PUT", "/cache/" + action + "/" + output, "valid", 9, http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			req.ContentLength = test.length
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != test.status {
				t.Fatalf("status = %d; want %d: %s", response.Code, test.status, response.Body)
			}
		})
	}
	if output, path, err := store.Get(context.Background(), action); err != nil || output != "" || path != "" {
		t.Fatalf("invalid upload entered server cache: %q, %q, %v", output, path, err)
	}
}

func TestConcurrentClients(t *testing.T) {
	store := newLocal(t)
	server := httptest.NewServer(NewHandler(&gocache.Server{Get: store.Get, Put: store.Put}))
	defer server.Close()
	uploader := newClient(t, server.URL, t.TempDir())
	downloader := newClient(t, server.URL, t.TempDir())
	data := []byte("shared immutable output")
	output := hashID(data)
	var group sync.WaitGroup
	errors := make(chan error, 16)
	for i := range 16 {
		group.Go(func() {
			action := hashID([]byte(fmt.Sprintf("action-%d", i)))
			_, err := uploader.Put(context.Background(), gocache.Object{
				ActionID: action, OutputID: output, Size: int64(len(data)), Body: bytes.NewReader(data),
			})
			if err == nil {
				var got string
				got, _, err = downloader.Get(context.Background(), action)
				if err == nil && got != output {
					err = fmt.Errorf("action %d: output = %q; want %q", i, got, output)
				}
			}
			errors <- err
		})
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Error(err)
		}
	}
}

func TestURLBasePath(t *testing.T) {
	store := newLocal(t)
	server := httptest.NewServer(http.StripPrefix("/prefix", NewHandler(&gocache.Server{Get: store.Get, Put: store.Put})))
	defer server.Close()
	first := newClient(t, server.URL+"/prefix/", t.TempDir())
	second := newClient(t, server.URL+"/prefix", t.TempDir())
	data := []byte("under a reverse proxy")
	action, output := hashID([]byte(t.Name())), hashID(data)
	if _, err := first.Put(context.Background(), gocache.Object{
		ActionID: action, OutputID: output, Size: int64(len(data)), Body: bytes.NewReader(data),
	}); err != nil {
		t.Fatal(err)
	}
	if got, _, err := second.Get(context.Background(), action); err != nil || got != output {
		t.Fatalf("Get under base path = %q, %v", got, err)
	}
}

func TestRejectInvalidURLs(t *testing.T) {
	for _, serverURL := range []string{"server:5970", "ftp://server", "http://", "http://server?query=1", "http://server#fragment"} {
		if client, err := NewClient(serverURL, t.TempDir()); err == nil {
			client.Close()
			t.Errorf("accepted invalid URL %q", serverURL)
		}
	}
}
