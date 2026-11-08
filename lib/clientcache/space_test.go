package clientcache

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestCapacityErrors(t *testing.T) {
	for _, test := range []struct {
		err  error
		full bool
	}{
		{nil, false},
		{&os.PathError{Op: "write", Path: "artifact", Err: syscall.ENOSPC}, true},
		{fmt.Errorf("write artifact: %w", syscall.EDQUOT), true},
		{os.ErrPermission, false},
	} {
		if got := IsCapacityError(test.err); got != test.full {
			t.Errorf("IsCapacityError(%v) = %v, want %v", test.err, got, test.full)
		}
	}
}

func TestCloseDoesNotWriteAccounting(t *testing.T) {
	directory := t.TempDir()
	session, err := New(directory, DefaultLimit)
	if err != nil {
		t.Fatal(err)
	}
	// Make accounting writes fail without requiring a privileged full tmpfs.
	// Releasing build pins must not depend on those writes succeeding.
	if err := os.Mkdir(filepath.Join(directory, ".dirty"), 0700); err != nil {
		t.Fatal(err)
	}
	buildDir := session.buildDir
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(buildDir); !os.IsNotExist(err) {
		t.Fatalf("build directory was not released: %v", err)
	}
}
