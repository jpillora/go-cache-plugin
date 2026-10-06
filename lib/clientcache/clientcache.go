// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package clientcache keeps a bounded LFU cache and pins artifacts for the
// lifetime of each build, so eviction cannot invalidate paths handed to Go.
package clientcache

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creachadair/atomicfile"
	"github.com/creachadair/gocache"
	"github.com/creachadair/gocache/cachedir"
	"github.com/gofrs/flock"
)

// DefaultLimit retains up to 2 GiB of persistent client cache entries.
const DefaultLimit int64 = 2 << 30

// Session shares a persistent cache with other processes. Its build directory
// contains hard links to artifacts returned to Go; Close removes those links.
type Session struct {
	directory  string
	buildDir   string
	limit      int64
	persistent *cachedir.Dir
	staging    *cachedir.Dir
	cacheLock  *flock.Flock
	buildLock  *flock.Flock
	operations sync.RWMutex
	mutex      sync.Mutex
	closed     bool
}

type frequency struct {
	Count uint64 `json:"count"`
	Used  int64  `json:"used"`
}

type candidate struct {
	id, path string
	bytes    int64
	frequency
	actions []string
}

// New opens a cache, repairs its byte accounting, evicts excess entries, and
// removes pins left by processes that exited without closing their session.
// A zero limit keeps artifacts only for the current build.
func New(directory string, limit int64) (*Session, error) {
	if limit < 0 {
		return nil, errors.New("cache size limit must not be negative")
	}
	absDir, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	persistent, err := cachedir.New(absDir)
	if err != nil {
		return nil, err
	}
	s := &Session{directory: absDir, limit: limit, persistent: persistent, cacheLock: flock.New(filepath.Join(absDir, ".cache.lock"))}
	err = s.locked(func() error {
		if err := s.removeAbandonedBuilds(); err != nil {
			return err
		}
		if err := s.pruneOrphans(); err != nil {
			return err
		}
		bytes, err := s.measure()
		if err != nil {
			return err
		}
		if err := s.resize(bytes); err != nil {
			return err
		}
		builds := filepath.Join(absDir, "builds")
		if err := os.MkdirAll(builds, 0700); err != nil {
			return err
		}
		s.buildDir, err = os.MkdirTemp(builds, "build-")
		if err != nil {
			return err
		}
		s.buildLock = flock.New(filepath.Join(s.buildDir, ".lock"))
		if err := s.buildLock.Lock(); err != nil {
			return err
		}
		s.staging, err = cachedir.New(s.buildDir)
		return err
	})
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Get returns a build-pinned path and records one use of the output artifact.
func (s *Session) Get(ctx context.Context, actionID string) (output, path string, err error) {
	if !isID(actionID) {
		return "", "", errors.New("invalid action ID")
	}
	s.operations.RLock()
	defer s.operations.RUnlock()
	if s.closed {
		return "", "", errors.New("client cache is closed")
	}
	err = s.locked(func() error {
		var lookupErr error
		output, path, lookupErr = s.staging.Get(ctx, actionID)
		if lookupErr == nil && output != "" {
			return s.touchIfPresent(output)
		}
		output, path, lookupErr = s.persistent.Get(ctx, actionID)
		if lookupErr != nil || output == "" {
			return lookupErr
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		// cachedir.Put copies the file if hard links are unavailable.
		_ = linkArtifact(path, artifactPath(s.buildDir, output))
		path, err = s.staging.Put(ctx, gocache.Object{ActionID: actionID, OutputID: output, Size: info.Size(), Body: file, ModTime: info.ModTime()})
		if err != nil {
			return err
		}
		return s.touchIfPresent(output)
	})
	return output, path, err
}

// Put stages the object before acquiring the shared lock, then retains it in
// the persistent cache if it fits. Network reads do not hold the shared lock.
func (s *Session) Put(ctx context.Context, object gocache.Object) (string, error) {
	if !isID(object.ActionID) || !isID(object.OutputID) || object.Size < 0 {
		return "", errors.New("invalid cache object")
	}
	s.operations.RLock()
	defer s.operations.RUnlock()
	if s.closed {
		return "", errors.New("client cache is closed")
	}
	path, err := s.staging.Put(ctx, object)
	if err != nil {
		return "", err
	}
	if s.limit == 0 || allocated(object.Size) > s.limit {
		return path, nil
	}
	err = s.locked(func() error {
		paths := []string{artifactPath(s.directory, object.OutputID), actionPath(s.directory, object.ActionID), s.frequencyPath(object.OutputID)}
		before, err := measurePaths(paths)
		if err != nil {
			return err
		}
		// A successful link lets cachedir skip a second copy of the object.
		_ = linkArtifact(path, paths[0])
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		object.Body = file
		if _, err := s.persistent.Put(ctx, object); err != nil {
			return err
		}
		if err := s.touch(object.OutputID); err != nil {
			return err
		}
		after, err := measurePaths(paths)
		if err != nil {
			return err
		}
		return s.account(after - before)
	})
	return path, err
}

// Close releases build pins. It must run in the cache protocol's close callback
// before replying, because Go may kill the plugin immediately after that reply.
func (s *Session) Close() error {
	s.operations.Lock()
	defer s.operations.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	if s.buildDir != "" {
		err = s.locked(func() error { return os.RemoveAll(s.buildDir) })
	}
	if s.buildLock != nil {
		err = errors.Join(err, s.buildLock.Close())
	}
	return errors.Join(err, s.cacheLock.Close())
}

func (s *Session) locked(fn func() error) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if err := s.cacheLock.Lock(); err != nil {
		return err
	}
	err := s.transaction(fn)
	return errors.Join(err, s.cacheLock.Unlock())
}

// A killed process or failed write can leave files newer than the size counter.
// Recover accounting before another process uses it to decide whether to evict.
func (s *Session) transaction(fn func() error) error {
	marker := filepath.Join(s.directory, ".dirty")
	if _, err := os.Stat(marker); err == nil {
		if err := s.pruneOrphans(); err != nil {
			return err
		}
		bytes, err := s.measure()
		if err != nil {
			return err
		}
		if err := s.resize(bytes); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Initial migration measures/trims first, which can free space on a full disk.
	if s.staging != nil {
		if err := os.WriteFile(marker, nil, 0600); err != nil {
			return err
		}
	}
	if err := fn(); err != nil {
		return err
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Session) touchIfPresent(output string) error {
	if _, err := os.Stat(artifactPath(s.directory, output)); errors.Is(err, fs.ErrNotExist) {
		return nil // This build still has a pin, but the shared entry was evicted.
	} else if err != nil {
		return err
	}
	before, err := measurePaths([]string{s.frequencyPath(output)})
	if err != nil {
		return err
	}
	if err := s.touch(output); err != nil {
		return err
	}
	after, err := measurePaths([]string{s.frequencyPath(output)})
	if err != nil {
		return err
	}
	return s.account(after - before)
}

func (s *Session) touch(output string) error {
	freq := s.readFrequency(output)
	freq.Count++
	freq.Used = time.Now().UnixNano()
	data, err := json.Marshal(freq)
	if err != nil {
		return err
	}
	path := s.frequencyPath(output)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return atomicfile.WriteData(path, data, 0644)
}

func (s *Session) readFrequency(output string) frequency {
	var freq frequency
	if data, err := os.ReadFile(s.frequencyPath(output)); err == nil {
		_ = json.Unmarshal(data, &freq)
	}
	return freq
}

func (s *Session) account(delta int64) error {
	data, err := os.ReadFile(filepath.Join(s.directory, ".size"))
	if err != nil {
		return err
	}
	bytes, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return err
	}
	bytes += delta
	return s.resize(bytes)
}

func (s *Session) resize(bytes int64) error {
	if bytes > s.limit {
		// Free space before replacing the counter, including during migration
		// from an old unbounded cache on a full disk.
		return s.trim(bytes)
	}
	return s.writeSize(bytes)
}

func (s *Session) writeSize(bytes int64) error {
	return atomicfile.WriteData(filepath.Join(s.directory, ".size"), []byte(strconv.FormatInt(bytes, 10)), 0644)
}

func (s *Session) trim(bytes int64) error {
	if bytes <= s.limit {
		return nil
	}
	entries := make(map[string]*candidate)
	if err := walkFiles(filepath.Join(s.directory, "output"), func(path string, info fs.FileInfo) error {
		id := filepath.Base(path)
		if !isID(id) {
			return fmt.Errorf("invalid cached output ID: %s", id)
		}
		freq := s.readFrequency(id)
		if freq.Used == 0 {
			freq.Used = info.ModTime().UnixNano()
		}
		metadata, err := measurePaths([]string{s.frequencyPath(id)})
		if err != nil {
			return err
		}
		entries[id] = &candidate{id: id, path: path, bytes: allocated(info.Size()) + metadata, frequency: freq}
		return nil
	}); err != nil {
		return err
	}
	if err := walkFiles(filepath.Join(s.directory, "action"), func(path string, info fs.FileInfo) error {
		id, err := readActionOutput(path)
		if err != nil {
			return err
		}
		if entry := entries[id]; entry != nil {
			entry.bytes += allocated(info.Size())
			entry.actions = append(entry.actions, path)
		}
		return nil
	}); err != nil {
		return err
	}
	var ordered []*candidate
	for _, entry := range entries {
		ordered = append(ordered, entry)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.Count != b.Count {
			return a.Count < b.Count
		}
		if a.Used != b.Used {
			return a.Used < b.Used
		}
		return a.id < b.id
	})
	// Leave headroom so a full cache does not need a directory scan on every put.
	target := s.limit - s.limit/10
	for _, entry := range ordered {
		if bytes <= target {
			break
		}
		for _, path := range append(entry.actions, entry.path, s.frequencyPath(entry.id)) {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		bytes -= entry.bytes
	}
	return s.writeSize(bytes)
}

func (s *Session) measure() (int64, error) {
	var bytes int64
	for _, name := range []string{"output", "action", "frequency"} {
		if err := walkFiles(filepath.Join(s.directory, name), func(_ string, info fs.FileInfo) error {
			bytes += allocated(info.Size())
			return nil
		}); err != nil {
			return 0, err
		}
	}
	return bytes, nil
}

func (s *Session) pruneOrphans() error {
	for _, name := range []string{"action", "output", "frequency"} {
		err := walkFiles(filepath.Join(s.directory, name), func(path string, _ fs.FileInfo) error {
			if strings.HasSuffix(path, ".aftmp") {
				return os.Remove(path)
			}
			if name == "output" {
				return nil
			}
			output := filepath.Base(path)
			if name == "action" {
				var err error
				output, err = readActionOutput(path)
				if err != nil {
					return os.Remove(path)
				}
			}
			if _, err := os.Stat(artifactPath(s.directory, output)); errors.Is(err, fs.ErrNotExist) {
				return os.Remove(path)
			} else {
				return err
			}
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) removeAbandonedBuilds() error {
	builds := filepath.Join(s.directory, "builds")
	entries, err := os.ReadDir(builds)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "build-") {
			continue
		}
		path := filepath.Join(builds, entry.Name())
		lock := flock.New(filepath.Join(path, ".lock"))
		acquired, err := lock.TryLock()
		if err == nil && acquired {
			err = os.RemoveAll(path)
		}
		err = errors.Join(err, lock.Close())
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) frequencyPath(id string) string {
	return filepath.Join(s.directory, "frequency", id[:2], id)
}

func artifactPath(directory, id string) string {
	return filepath.Join(directory, "output", id[:2], id)
}

func actionPath(directory, id string) string {
	return filepath.Join(directory, "action", id[:2], id)
}

func linkArtifact(source, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	return os.Link(source, target)
}

func readActionOutput(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 || !isID(fields[0]) {
		return "", fmt.Errorf("invalid action record %s", path)
	}
	return fields[0], nil
}

func isID(id string) bool {
	if len(id) != 64 || id != strings.ToLower(id) {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// Account for small metadata files rather than treating them as free. Round
// file sizes to typical 4 KiB filesystem allocations; directory/lock overhead
// is small and build pins are accounted separately from persistent entries.
func allocated(size int64) int64 { return (size + 4095) / 4096 * 4096 }

func measurePaths(paths []string) (int64, error) {
	var bytes int64
	for _, path := range paths {
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return 0, err
		}
		bytes += allocated(info.Size())
	}
	return bytes, nil
}

func walkFiles(root string, visit func(string, fs.FileInfo) error) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && path == root {
			return nil
		} else if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return visit(path, info)
	})
}
