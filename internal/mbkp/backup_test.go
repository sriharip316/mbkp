package mbkp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunFullBackupPreCanceledContext(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &Config{
		BackupDir: tmpDir,
		BackupBin: "mariabackup",
		StreamBin: "mbstream",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-canceled

	err := RunFullBackup(ctx, cfg)
	if err == nil {
		t.Fatal("expected RunFullBackup with pre-canceled context to fail")
	}

	// Verify no in-progress metadata was recorded
	meta, err := LoadMetadata(tmpDir)
	if err == nil && len(meta.Backups) > 0 {
		t.Errorf("expected no backups in catalog, got %d", len(meta.Backups))
	}
}

func TestRunIncrementalBackupPreCanceledContext(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &Config{
		BackupDir: tmpDir,
		BackupBin: "mariabackup",
		StreamBin: "mbstream",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-canceled

	err := RunIncrementalBackup(ctx, cfg, "parent-id")
	if err == nil {
		t.Fatal("expected RunIncrementalBackup with pre-canceled context to fail")
	}
}

func TestRunFullBackupCancelDuringStreaming(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &Config{
		BackupDir: tmpDir,
		BackupBin: "sleep",
		StreamBin: "cat",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// "sleep" will sleep for 10 seconds, but context timeout fires at 100ms
	// streamBackup should kill sleep and compressor, delete partial archive,
	// and record status: "failed" in SQLite.
	err := RunFullBackup(ctx, cfg)
	if err == nil {
		t.Fatal("expected RunFullBackup to fail on timeout")
	}

	meta, err := LoadMetadata(tmpDir)
	if err != nil {
		t.Fatalf("failed to load metadata: %v", err)
	}
	if len(meta.Backups) != 1 {
		t.Fatalf("expected 1 backup record, got %d", len(meta.Backups))
	}

	b := meta.Backups[0]
	if b.Status != "failed" {
		t.Errorf("expected backup status to be 'failed', got %q", b.Status)
	}

	// Verify partial archive file is cleaned up
	archive := filepath.Join(tmpDir, b.Path)
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Errorf("expected partial archive %s to be deleted, but it exists", archive)
	}

	// Verify no temporary directories (lsn_tmp_* or target_tmp_*) remain
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("failed to read backup dir: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() && (strings.HasPrefix(entry.Name(), "lsn_tmp_") || strings.HasPrefix(entry.Name(), "target_tmp_")) {
			t.Errorf("temporary directory %s was leaked", entry.Name())
		}
	}
}
