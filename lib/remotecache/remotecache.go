// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package remotecache transfers build-cache artifacts over HTTP and stages them
// on the client, so the Go toolchain never receives a server-local file path.
package remotecache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/creachadair/gocache"
	"github.com/creachadair/gocache/cachedir"
)

const outputIDHeader = "X-Go-Cache-Output-ID"

// NewHandler exposes the cache callbacks through an HTTP artifact API. GET
// /cache/{action} returns an artifact with its output ID in a response header;
// PUT /cache/{action}/{output} stores an artifact. IDs are SHA-256 hex strings.
// This API is intended for a trusted network, such as a Tailscale tailnet.
func NewHandler(cache *gocache.Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /cache/{action}", func(w http.ResponseWriter, r *http.Request) {
		action := r.PathValue("action")
		if !validID(action) {
			http.Error(w, "invalid action ID", http.StatusBadRequest)
			return
		}
		if cache.Get == nil {
			http.NotFound(w, r)
			return
		}
		ctx := gocache.WithLogf(r.Context(), cache.Logf)
		output, diskPath, err := cache.Get(ctx, action)
		if err != nil {
			http.Error(w, "cache lookup failed", http.StatusInternalServerError)
			return
		}
		if output == "" && diskPath == "" {
			http.NotFound(w, r)
			return
		}
		if !validID(output) {
			http.Error(w, "invalid cached output ID", http.StatusInternalServerError)
			return
		}
		file, err := os.Open(diskPath)
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		} else if err != nil {
			http.Error(w, "open cached artifact failed", http.StatusInternalServerError)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			http.Error(w, "invalid cached artifact", http.StatusInternalServerError)
			return
		}
		w.Header().Set(outputIDHeader, output)
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, output, info.ModTime(), file)
	})
	mux.HandleFunc("PUT /cache/{action}/{output}", func(w http.ResponseWriter, r *http.Request) {
		action, output := r.PathValue("action"), r.PathValue("output")
		if !validID(action) || !validID(output) {
			http.Error(w, "invalid action or output ID", http.StatusBadRequest)
			return
		}
		if cache.Put == nil {
			http.Error(w, "cache is read-only", http.StatusMethodNotAllowed)
			return
		}
		if r.ContentLength < 0 {
			http.Error(w, "Content-Length is required", http.StatusLengthRequired)
			return
		}
		_, err := cache.Put(gocache.WithLogf(r.Context(), cache.Logf), gocache.Object{
			ActionID: action,
			OutputID: output,
			Size:     r.ContentLength,
			Body:     checkBody(r.Body, output),
		})
		if err != nil {
			http.Error(w, "store cached artifact failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// Client implements the Get and Put callbacks of a gocache.Server using a local
// cache backed by a remote HTTP server. It requires no S3 credentials.
type Client struct {
	baseURL *url.URL
	local   *cachedir.Dir
	http    *http.Client

	// Logf, if set, reports remote upload failures. A failed upload still leaves
	// a usable local cache entry, so transient outages do not break builds.
	Logf func(string, ...any)
}

// NewClient constructs a client with an absolute local cache directory. The URL
// may include a base path, but must use HTTP or HTTPS with no query or fragment.
func NewClient(serverURL, cacheDir string) (*Client, error) {
	base, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("parse cache server URL: %w", err)
	}
	if (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("cache server URL must be http(s)://host[:port][/path] with no query or fragment")
	}
	absDir, err := filepath.Abs(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("resolve client cache directory: %w", err)
	}
	local, err := cachedir.New(absDir)
	if err != nil {
		return nil, fmt.Errorf("create client cache: %w", err)
	}
	return &Client{baseURL: base, local: local, http: &http.Client{Timeout: time.Minute}}, nil
}

// Close releases idle HTTP connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// Get checks the local cache first, then downloads a remote hit into the local
// directory before returning its path. A remote 404 is an ordinary cache miss.
func (c *Client) Get(ctx context.Context, actionID string) (string, string, error) {
	if !validID(actionID) {
		return "", "", errors.New("invalid action ID")
	}
	if output, path, err := c.local.Get(ctx, actionID); err == nil && output != "" {
		return output, path, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL.JoinPath("cache", actionID).String(), nil)
	if err != nil {
		return "", "", err
	}
	response, err := c.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("read remote cache: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return "", "", nil
	} else if response.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("read remote cache: %s", response.Status)
	}
	output := response.Header.Get(outputIDHeader)
	if !validID(output) || response.ContentLength < 0 {
		return "", "", errors.New("remote cache returned invalid artifact metadata")
	}
	mtime, _ := http.ParseTime(response.Header.Get("Last-Modified"))
	path, err := c.local.Put(ctx, gocache.Object{
		ActionID: actionID,
		OutputID: output,
		Size:     response.ContentLength,
		Body:     checkBody(response.Body, output),
		ModTime:  mtime,
	})
	if err != nil {
		return "", "", fmt.Errorf("stage remote artifact: %w", err)
	}
	return output, path, nil
}

// Put stores the artifact locally, then uploads it. Go always receives the
// local path, including when an upload fails. Upload failures are logged.
func (c *Client) Put(ctx context.Context, object gocache.Object) (string, error) {
	if !validID(object.ActionID) || !validID(object.OutputID) || object.Size < 0 {
		return "", errors.New("invalid cache object")
	}
	object.Body = checkBody(object.Body, object.OutputID)
	path, err := c.local.Put(ctx, object)
	if err != nil {
		return "", err
	}
	if err := c.upload(ctx, object, path); err != nil && c.Logf != nil {
		c.Logf("upload cache action %s: %v", object.ActionID, err)
	}
	return path, nil
}

func (c *Client) upload(ctx context.Context, object gocache.Object, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var body io.Reader = file
	if object.Size == 0 {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL.JoinPath("cache", object.ActionID, object.OutputID).String(), body)
	if err != nil {
		return err
	}
	req.ContentLength = object.Size
	req.Header.Set("Content-Type", "application/octet-stream")
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("remote cache: %s", response.Status)
	}
	return nil
}

func validID(id string) bool {
	if len(id) != 2*sha256.Size || id != strings.ToLower(id) {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// checksumReader fails before cachedir commits an object if its contents do not
// match the content-addressed output ID. cachedir also checks the byte count.
type checksumReader struct {
	reader io.Reader
	hash   hash.Hash
	want   string
}

func checkBody(reader io.Reader, outputID string) io.Reader {
	return &checksumReader{reader: reader, hash: sha256.New(), want: outputID}
}

func (r *checksumReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.hash.Write(p[:n])
	if err == io.EOF && hex.EncodeToString(r.hash.Sum(nil)) != r.want {
		return n, errors.New("artifact checksum mismatch")
	}
	return n, err
}
