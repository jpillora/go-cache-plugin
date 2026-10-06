// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gobuild

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/creachadair/gocache"
	"github.com/creachadair/gocache/cachedir"
	"github.com/jpillora/go-cache-plugin/lib/s3util"
)

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
	_, err = cache.Put(ctx, gocache.Object{
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
