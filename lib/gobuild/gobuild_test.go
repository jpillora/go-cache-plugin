// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gobuild

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/creachadair/gocache"
	"github.com/creachadair/gocache/cachedir"
	"github.com/creachadair/taskgroup"
	"github.com/jpillora/go-cache-plugin/lib/s3util"
)

type failingLocal struct{ getErr, putErr error }

func (f failingLocal) Get(context.Context, string) (string, string, error) {
	return "", "", f.getErr
}

func (f failingLocal) Put(context.Context, gocache.Object) (string, error) {
	return "", f.putErr
}

func TestFullLocalCacheIsAMiss(t *testing.T) {
	for _, test := range []struct {
		name     string
		local    failingLocal
		requests int64
		wantErr  error
	}{
		{"session full", failingLocal{getErr: syscall.ENOSPC}, 0, nil},
		{"staging full", failingLocal{putErr: fmt.Errorf("write: %w", syscall.ENOSPC)}, 2, nil},
		{"quota exhausted", failingLocal{putErr: syscall.EDQUOT}, 2, nil},
		{"permissions remain errors", failingLocal{putErr: os.ErrPermission}, 2, os.ErrPermission},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int64
			data := "remote artifact"
			output := fmt.Sprintf("%x", sha256.Sum256([]byte(data)))
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if strings.Contains(r.URL.Path, "/action/") {
					fmt.Fprintf(w, "%s %d", output, time.Now().UnixNano())
				} else {
					io.WriteString(w, data)
				}
			}))
			defer remote.Close()
			cache := &S3Cache{Local: test.local, S3Client: &s3util.Client{
				Client: s3.New(s3.Options{Region: "test", BaseEndpoint: aws.String(remote.URL), UsePathStyle: true, Credentials: aws.AnonymousCredentials{}}), Bucket: "test",
			}}
			id, path, err := cache.Get(t.Context(), output)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Get error = %v, want %v", err, test.wantErr)
			}
			if test.wantErr == nil && (id != "" || path != "") {
				t.Fatalf("full cache returned a hit: %q, %q", id, path)
			}
			if got := requests.Load(); got != test.requests {
				t.Fatalf("S3 requests = %d, want %d", got, test.requests)
			}
		})
	}
}

func TestUploadSurvivesRequestCancellation(t *testing.T) {
	objectStarted := make(chan struct{})
	releaseObject := make(chan struct{})
	var actionUploaded atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if strings.Contains(r.URL.Path, "/output/") {
			close(objectStarted)
			<-releaseObject
		} else if strings.Contains(r.URL.Path, "/action/") {
			actionUploaded.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	local, err := cachedir.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cache := &S3Cache{
		Local: local,
		S3Client: &s3util.Client{
			Client: s3.New(s3.Options{
				Region: "test", BaseEndpoint: aws.String(server.URL),
				UsePathStyle: true, Credentials: aws.AnonymousCredentials{},
			}),
			Bucket: "test-bucket",
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	data := "request-independent artifact"
	pin, err := cache.Put(ctx, gocache.Object{
		ActionID: fmt.Sprintf("%x", sha256.Sum256([]byte(t.Name()))),
		OutputID: fmt.Sprintf("%x", sha256.Sum256([]byte(data))),
		Size:     int64(len(data)),
		Body:     strings.NewReader(data),
	})
	if err != nil {
		close(releaseObject)
		t.Fatal(err)
	}
	select {
	case <-objectStarted:
	case <-time.After(5 * time.Second):
		close(releaseObject)
		t.Fatal("object upload did not start")
	}
	// Releasing a request's local pin must not discard an in-flight upload.
	if err := os.Remove(pin); err != nil {
		close(releaseObject)
		t.Fatal(err)
	}
	// HTTP request contexts end as soon as their response is sent. Background
	// uploads must retain an independent context for both object and action.
	cancel()
	close(releaseObject)
	if err := cache.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !actionUploaded.Load() {
		t.Fatal("action upload was lost when the request context ended")
	}
}

// A queued upload may not start until after the originating HTTP request ends.
func TestQueuedUploadSurvivesReleasedPin(t *testing.T) {
	data := "artifact whose request ended before upload started"
	var uploaded atomic.Bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/output/") {
			if string(body) != data {
				t.Errorf("uploaded bytes = %q", body)
			}
			uploaded.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer remote.Close()
	local, err := cachedir.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cache := &S3Cache{Local: local, S3Client: &s3util.Client{
		Client: s3.New(s3.Options{Region: "test", BaseEndpoint: aws.String(remote.URL), UsePathStyle: true, Credentials: aws.AnonymousCredentials{}}), Bucket: "test",
	}}
	cache.init()
	// Hold the scheduler, independently of network timing.
	var queued taskgroup.Task
	cache.start = func(task taskgroup.Task) { queued = task }
	pin, err := cache.Put(t.Context(), gocache.Object{ActionID: fmt.Sprintf("%x", sha256.Sum256([]byte(t.Name()))), OutputID: fmt.Sprintf("%x", sha256.Sum256([]byte(data))), Size: int64(len(data)), Body: strings.NewReader(data)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(pin); err != nil {
		t.Fatal(err)
	}
	if queued == nil {
		t.Fatal("upload not scheduled")
	}
	if err := queued(); err != nil {
		t.Fatal(err)
	}
	if !uploaded.Load() {
		t.Fatal("upload was lost after request released its pin")
	}
}
