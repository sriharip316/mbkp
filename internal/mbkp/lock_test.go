package mbkp

import (
	"strings"
	"testing"
)

func TestAcquireLockExclusive(t *testing.T) {
	tmpDir := t.TempDir()

	release, err := AcquireLock(tmpDir)
	if err != nil {
		t.Fatalf("first AcquireLock failed: %v", err)
	}

	// flock conflicts are per open file description, so a second acquire in
	// the same process fails exactly like a second process would.
	_, err = AcquireLock(tmpDir)
	if err == nil {
		t.Fatal("expected second AcquireLock to fail while the lock is held")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("expected conflict error to mention a running operation, got: %v", err)
	}

	if err := release(); err != nil {
		t.Fatalf("release failed: %v", err)
	}

	// After release the lock must be acquirable again.
	release2, err := AcquireLock(tmpDir)
	if err != nil {
		t.Fatalf("AcquireLock after release failed: %v", err)
	}
	if err := release2(); err != nil {
		t.Fatalf("second release failed: %v", err)
	}
}

func TestAcquireLockIndependentDirs(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()

	releaseA, err := AcquireLock(dirA)
	if err != nil {
		t.Fatalf("AcquireLock on dirA failed: %v", err)
	}
	defer func() { _ = releaseA() }()

	// Locks are per backup directory: a different dir must not be blocked.
	releaseB, err := AcquireLock(dirB)
	if err != nil {
		t.Fatalf("AcquireLock on independent dirB should not be blocked by dirA: %v", err)
	}
	if err := releaseB(); err != nil {
		t.Fatalf("release of dirB failed: %v", err)
	}
}
