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

// TestRunFullBackupMarksFailedWhenTargetDirCreationFails pins a backup ID so
// the per-run target directory collides with a regular file: os.MkdirAll then
// fails after the in_progress row has been inserted — one of the failure
// paths that used to orphan the row in in_progress (which purge would retain
// for the whole retention window).
func TestRunFullBackupMarksFailedWhenTargetDirCreationFails(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &Config{BackupDir: tmpDir}

	orig := backupIDGenerator
	backupIDGenerator = func(prefix string) string { return prefix + "seamtest" }
	defer func() { backupIDGenerator = orig }()

	// Block target_tmp_full_seamtest so os.MkdirAll fails after AddBackup.
	if err := os.WriteFile(filepath.Join(tmpDir, "target_tmp_full_seamtest"), []byte("x"), 0644); err != nil {
		t.Fatalf("failed to create blocking file: %v", err)
	}

	if err := RunFullBackup(context.Background(), cfg); err == nil {
		t.Fatal("expected RunFullBackup to fail when the target directory cannot be created")
	}

	meta, err := LoadMetadata(tmpDir)
	if err != nil {
		t.Fatalf("failed to load metadata: %v", err)
	}
	if len(meta.Backups) != 1 {
		t.Fatalf("expected exactly one catalog row, got %d", len(meta.Backups))
	}
	b := meta.Backups[0]
	if b.Status != "failed" {
		t.Errorf("expected status 'failed' after the failure, got %q", b.Status)
	}
	if b.EndTime.IsZero() {
		t.Error("expected end_time to be recorded on the failed row")
	}
}

// TestRunIncrementalBackupMarksFailedWhenTargetDirCreationFails covers the
// same post-insert failure transition for incremental backups.
func TestRunIncrementalBackupMarksFailedWhenTargetDirCreationFails(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &Config{BackupDir: tmpDir}

	orig := backupIDGenerator
	backupIDGenerator = func(prefix string) string { return prefix + "seamtest" }
	defer func() { backupIDGenerator = orig }()

	// A completed parent with stored checkpoints so the incremental gets past
	// parent selection and inserts its own in_progress row.
	now := time.Now()
	parent := BackupMetadata{
		ID:          "full_parent",
		Type:        "full",
		Status:      "completed",
		StartTime:   now.Add(-time.Hour),
		EndTime:     now,
		Path:        "full_parent.xbstream.lz4",
		Checkpoints: "backup_type = full-backuped\nfrom_lsn = 0\nto_lsn = 100\n",
	}
	if err := AddBackup(tmpDir, parent); err != nil {
		t.Fatalf("failed to add parent backup: %v", err)
	}

	// Block target_tmp_inc_seamtest so os.MkdirAll fails after AddBackup.
	if err := os.WriteFile(filepath.Join(tmpDir, "target_tmp_inc_seamtest"), []byte("x"), 0644); err != nil {
		t.Fatalf("failed to create blocking file: %v", err)
	}

	if err := RunIncrementalBackup(context.Background(), cfg, ""); err == nil {
		t.Fatal("expected RunIncrementalBackup to fail when the target directory cannot be created")
	}

	inc, err := GetBackupByID(tmpDir, "inc_seamtest")
	if err != nil {
		t.Fatalf("incremental row missing from catalog: %v", err)
	}
	if inc.Status != "failed" {
		t.Errorf("expected status 'failed' after the failure, got %q", inc.Status)
	}
	if inc.EndTime.IsZero() {
		t.Error("expected end_time to be recorded on the failed row")
	}
}
