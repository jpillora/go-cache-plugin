// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"errors"
	"net/http"

	"github.com/creachadair/gocache"
	"github.com/jpillora/go-cache-plugin/lib/clientcache"
)

type localSessionKey struct{}

// boundedLocal routes cache operations to build/request-scoped pins. Only the
// persistent LFU cache is size bounded; live builds and uploads retain their
// files until they finish, even when eviction removes the persistent entry.
type boundedLocal struct{ root *clientcache.Session }

func (b *boundedLocal) Get(ctx context.Context, action string) (string, string, error) {
	session, ok := ctx.Value(localSessionKey{}).(*clientcache.Session)
	if !ok {
		return "", "", errors.New("missing local cache session")
	}
	return session.Get(ctx, action)
}

func (b *boundedLocal) Put(ctx context.Context, object gocache.Object) (string, error) {
	session, ok := ctx.Value(localSessionKey{}).(*clientcache.Session)
	if !ok {
		return "", errors.New("missing local cache session")
	}
	return session.Put(ctx, object)
}

type cacheServer struct {
	*gocache.Server
	local *boundedLocal
}

func (s *cacheServer) session(ctx context.Context) (context.Context, func(), error) {
	if s.local == nil {
		return ctx, func() {}, nil
	}
	session, err := s.local.root.NewSession()
	if err != nil {
		return nil, nil, err
	}
	return context.WithValue(ctx, localSessionKey{}, session), func() {
		if err := session.Close(); err != nil {
			vprintf("release local cache pins: %v", err)
		}
	}, nil
}

func (s *cacheServer) handler(next http.Handler) http.Handler {
	if s.local == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, release, err := s.session(r.Context())
		if err != nil {
			http.Error(w, "open local cache session failed", http.StatusInternalServerError)
			return
		}
		defer release()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
