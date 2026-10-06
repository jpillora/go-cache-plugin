// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package clientcache

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creachadair/gocache"
	"github.com/creachadair/gocache/cachedir"
)

func idFor(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func openSession(t *testing.T, directory string, limit int64) *Session {
	t.Helper()
	session, err := New(directory, limit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	return session
}

func putArtifact(t *testing.T, session *Session, name string, size int) (string, string, []byte) {
	t.Helper()
	data := bytes.Repeat([]byte(name), size/len(name))
	action := idFor([]byte("action-" + name))
	path, err := session.Put(t.Context(), gocache.Object{
		ActionID: action, OutputID: idFor(data), Size: int64(len(data)), Body: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatal(err)
	}
	return action, path, data
}

func TestLFUEvictionAndBuildPins(t *testing.T) {
	// Each object plus its action and frequency metadata occupies 16 KiB.
	// The cache can retain three; adding a fourth must evict a cold object.
	directory := t.TempDir()
	first := openSession(t, directory, 56<<10)
	hot, hotPath, _ := putArtifact(t, first, "a", 8<<10)
	for range 5 {
		if _, _, err := first.Get(t.Context(), hot); err != nil {
			t.Fatal(err)
		}
	}
	cold, coldPath, coldData := putArtifact(t, first, "b", 8<<10)
	putArtifact(t, first, "c", 8<<10)
	second := openSession(t, directory, 56<<10)
	putArtifact(t, second, "d", 8<<10)
	if got, _, err := second.Get(t.Context(), hot); err != nil || got == "" {
		t.Fatalf("frequently used, older artifact was evicted: %q, %v", got, err)
	}
	if got, _, err := second.Get(t.Context(), cold); err != nil || got != "" {
		t.Fatalf("cold artifact remained in shared cache: %q, %v", got, err)
	}
	// The victim's path remains valid in the build that was already using it.
	if got, err := os.ReadFile(coldPath); err != nil || !bytes.Equal(got, coldData) {
		t.Fatalf("eviction invalidated an active build: %v", err)
	}
	if got, path, err := first.Get(t.Context(), cold); err != nil || got == "" || path != coldPath {
		t.Fatalf("pinned cache hit failed: %q, %q, %v", got, path, err)
	}
	if size, err := second.measure(); err != nil || size > 56<<10 {
		t.Fatalf("persistent cache exceeded budget: %d, %v", size, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{hotPath, coldPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("build pin was not removed: %s, %v", path, err)
		}
	}
	if got, _, err := second.Get(t.Context(), hot); err != nil || got == "" {
		t.Fatalf("closing a build removed shared cache data: %q, %v", got, err)
	}
}

func TestPersistentFrequencyAndSharedArtifacts(t *testing.T) {
	directory := t.TempDir()
	first := openSession(t, directory, DefaultLimit)
	action, path, data := putArtifact(t, first, "a", 8<<10)
	_, _, err := first.Get(t.Context(), action)
	if err != nil {
		t.Fatal(err)
	}
	output := idFor(data)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	persistentInfo, err := os.Stat(artifactPath(directory, output))
	if err != nil || !os.SameFile(info, persistentInfo) {
		t.Fatalf("build pin duplicated artifact data: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := openSession(t, directory, DefaultLimit)
	if count := second.readFrequency(output).Count; count != 2 {
		t.Fatalf("frequency did not survive reopening: %d", count)
	}
	if got, _, err := second.Get(t.Context(), action); err != nil || got != output {
		t.Fatalf("persistent hit = %q, %v", got, err)
	}
}

func TestZeroLimitAndOversizedArtifacts(t *testing.T) {
	for _, limit := range []int64{0, 16 << 10} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			session := openSession(t, t.TempDir(), limit)
			action, path, data := putArtifact(t, session, "a", 32<<10)
			if output, _, err := session.Get(t.Context(), action); err != nil || output != idFor(data) {
				t.Fatalf("working artifact unavailable: %q, %v", output, err)
			}
			if bytes, err := session.measure(); err != nil || bytes != 0 {
				t.Fatalf("oversized artifact was retained: %d, %v", bytes, err)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("working artifact leaked after close: %v", err)
			}
		})
	}
}

func TestMigrationRepairsAccountingAndTrimsOldCache(t *testing.T) {
	directory := t.TempDir()
	old, err := cachedir.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c", "d"} {
		data := bytes.Repeat([]byte(name), 16<<10)
		if _, err := old.Put(t.Context(), gocache.Object{
			ActionID: idFor([]byte(name)), OutputID: idFor(data), Size: int64(len(data)), Body: bytes.NewReader(data),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, ".size"), []byte("incorrect"), 0644); err != nil {
		t.Fatal(err)
	}
	session := openSession(t, directory, 32<<10)
	if bytes, err := session.measure(); err != nil || bytes > 32<<10 {
		t.Fatalf("old unbounded cache was not trimmed: %d, %v", bytes, err)
	}
}

func TestConcurrentSessions(t *testing.T) {
	directory := t.TempDir()
	first := openSession(t, directory, 32<<10)
	second := openSession(t, directory, 32<<10)
	var group sync.WaitGroup
	for i := range 20 {
		group.Go(func() {
			session := first
			if i%2 == 0 {
				session = second
			}
			data := bytes.Repeat([]byte{byte(i)}, 8<<10)
			action := idFor([]byte(fmt.Sprint(i)))
			path, err := session.Put(t.Context(), gocache.Object{
				ActionID: action, OutputID: idFor(data), Size: int64(len(data)), Body: bytes.NewReader(data),
			})
			if err != nil {
				t.Error(err)
				return
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
				t.Errorf("concurrent eviction invalidated a build: %v", err)
			}
		})
	}
	group.Wait()
	if size, err := first.measure(); err != nil || size > 32<<10 {
		t.Fatalf("concurrent writes exceeded budget: %d, %v", size, err)
	}
}

func TestInterruptedWriteAccountingIsRecovered(t *testing.T) {
	directory := t.TempDir()
	session := openSession(t, directory, 32<<10)
	action, _, _ := putArtifact(t, session, "a", 8<<10)
	// Simulate a process dying after publishing artifacts but before recording
	// their size. An already-running session must recover without reopening.
	old, err := cachedir.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("b"), 32<<10)
	if _, err := old.Put(t.Context(), gocache.Object{
		ActionID: idFor([]byte("interrupted")), OutputID: idFor(data), Size: int64(len(data)), Body: bytes.NewReader(data),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".dirty"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := session.Get(t.Context(), action); err != nil {
		t.Fatal(err)
	}
	if size, err := session.measure(); err != nil || size > 32<<10 {
		t.Fatalf("interrupted write escaped accounting: %d, %v", size, err)
	}
}

func TestCacheProcess(t *testing.T) {
	mode := os.Getenv("GO_CACHE_LFU_TEST_PROCESS")
	if mode == "" {
		return
	}
	session, err := New(os.Getenv("GO_CACHE_LFU_DIRECTORY"), DefaultLimit)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for range 20 {
		if output, _, err := session.Get(context.Background(), os.Getenv("GO_CACHE_LFU_ACTION")); err != nil || output == "" {
			fmt.Fprintf(os.Stderr, "Get = %q, %v\n", output, err)
			os.Exit(1)
		}
	}
	if mode == "wait" {
		fmt.Println(session.buildDir)
		time.Sleep(time.Hour)
	}
	if err := session.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func cacheProcess(t *testing.T, directory, action, mode string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCacheProcess$")
	cmd.Env = append(os.Environ(), "GO_CACHE_LFU_TEST_PROCESS="+mode, "GO_CACHE_LFU_DIRECTORY="+directory, "GO_CACHE_LFU_ACTION="+action)
	return cmd
}

func TestFrequencyUpdatesAcrossProcesses(t *testing.T) {
	directory := t.TempDir()
	session := openSession(t, directory, DefaultLimit)
	action, _, data := putArtifact(t, session, "a", 8<<10)
	var group sync.WaitGroup
	for range 2 {
		group.Go(func() {
			if output, err := cacheProcess(t, directory, action, "read").CombinedOutput(); err != nil {
				t.Errorf("child process: %v, %s", err, output)
			}
		})
	}
	group.Wait()
	if count := session.readFrequency(idFor(data)).Count; count != 41 {
		t.Fatalf("concurrent process frequency updates were lost: %d", count)
	}
}

func TestAbandonedBuildPinsAreRecovered(t *testing.T) {
	directory := t.TempDir()
	first := openSession(t, directory, DefaultLimit)
	action, _, _ := putArtifact(t, first, "a", 8<<10)
	cmd := cacheProcess(t, directory, action, "wait")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	abandoned := strings.TrimSpace(line)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	openSession(t, directory, DefaultLimit)
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("killed build's pins were not removed: %v", err)
	}
	if _, err := os.Stat(first.buildDir); err != nil {
		t.Fatalf("active build's pins were removed: %v", err)
	}
}

func TestParseLimit(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int64
	}{
		{"0", 0}, {"1024", 1024}, {"2GiB", 2 << 30}, {"2GB", 2_000_000_000}, {"64KiB", 64 << 10},
		{"-1", -1}, {"bogus", -1}, {"9999999999999999GiB", -1},
	} {
		got, err := ParseLimit(test.value)
		if test.want < 0 {
			if err == nil {
				t.Errorf("accepted invalid size %q", test.value)
			}
		} else if err != nil || got != test.want {
			t.Errorf("ParseLimit(%q) = %d, %v; want %d", test.value, got, err, test.want)
		}
	}
}
