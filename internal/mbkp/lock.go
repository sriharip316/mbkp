package mbkp

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// lockFileName is the advisory lock file serializing mutating mbkp operations
// on a backup directory. It is created once and never removed: flock locks are
// bound to the open file description, so replacing (unlinking + recreating)
// the file would allow two processes to hold locks on different inodes.
const lockFileName = ".mbkp.lock"

// AcquireLock takes an exclusive, non-blocking advisory lock on the backup
// directory, serializing mbkp operations that mutate it (backups, restores,
// PITR, binlog archiving, purges). The lock is kernel-managed: it is released
// when the returned release function is called and automatically when the
// process exits, so a killed run never leaves a stale lock behind.
//
// It must be taken exactly once per process, at the CLI entry point of a
// mutating command — never inside internal/mbkp functions, which call each
// other in one process (e.g. RunPITR calls RestoreBackup) and would deadlock
// against their own lock on a second file description.
//
// The holder's PID is written into the lock file for diagnostics; any failure
// to acquire or create the lock fails closed.
func AcquireLock(backupDir string) (release func() error, err error) {
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create backup directory: %w", err)
	}

	lock, err := os.OpenFile(filepath.Join(backupDir, lockFileName), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open lock file in %s: %w", backupDir, err)
	}

	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := readLockHolder(lock)
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if holder != "" {
				return nil, fmt.Errorf("another mbkp operation is already running on %s (lock held by PID %s)", backupDir, holder)
			}
			return nil, fmt.Errorf("another mbkp operation is already running on %s", backupDir)
		}
		return nil, fmt.Errorf("failed to lock %s: %w", backupDir, err)
	}

	// Record our PID so a colliding run can report who holds the lock.
	if err := lock.Truncate(0); err != nil {
		slog.Warn("failed to truncate lock file", "error", err)
	} else if _, err := lock.WriteString(strconv.Itoa(os.Getpid())); err != nil {
		slog.Warn("failed to write PID to lock file", "error", err)
	}

	return func() error {
		releaseErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		closeErr := lock.Close()
		if releaseErr != nil {
			return fmt.Errorf("failed to release lock on %s: %w", backupDir, releaseErr)
		}
		return closeErr
	}, nil
}

// readLockHolder best-effort reads the PID recorded in the lock file by its
// current holder. Returns "" when the file is empty or holds no valid PID.
func readLockHolder(lock *os.File) string {
	data, err := os.ReadFile(lock.Name())
	if err != nil {
		return ""
	}
	holder := string(data)
	if _, err := strconv.Atoi(holder); err != nil {
		return ""
	}
	return holder
}
