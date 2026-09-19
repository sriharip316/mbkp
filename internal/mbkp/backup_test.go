package mbkp

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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

// TestRunFullBackupRemovesArchiveWhenCompressorStartFails forces the one
// streamBackup failure path whose cleanup used to be inconsistent: when the
// compressor process cannot even start, the (zero-byte) archive file already
// exists and must be removed, like every other failure path does.
func TestRunFullBackupRemovesArchiveWhenCompressorStartFails(t *testing.T) {
	tmpDir := t.TempDir()
	// "true" starts fine and exits 0 immediately, so the pipeline reaches the
	// compressor start, which fails on the injected missing binary.
	cfg := &Config{BackupDir: tmpDir, BackupBin: "true"}

	origID := backupIDGenerator
	backupIDGenerator = func(prefix string) string { return prefix + "seamtest" }
	defer func() { backupIDGenerator = origID }()

	origComp := compressorDetector
	compressorDetector = func() Compressor {
		return Compressor{
			Name:           "mbkp-missing-compressor",
			Ext:            ".xbstream.gz",
			CompressArgs:   []string{"-c"},
			DecompressArgs: []string{"-dc"},
		}
	}
	defer func() { compressorDetector = origComp }()

	err := RunFullBackup(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected RunFullBackup to fail when the compressor cannot start")
	}
	if !strings.Contains(err.Error(), "mbkp-missing-compressor") {
		t.Errorf("expected the error to name the compressor, got: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(tmpDir, "full_seamtest.xbstream.gz")); !os.IsNotExist(statErr) {
		t.Error("expected the archive file to be removed when the compressor fails to start")
	}

	meta, err := LoadMetadata(tmpDir)
	if err != nil {
		t.Fatalf("failed to load metadata: %v", err)
	}
	if len(meta.Backups) != 1 || meta.Backups[0].Status != "failed" {
		t.Errorf("expected a single 'failed' catalog row, got %+v", meta.Backups)
	}
}

// TestRunFullBackupFailsFastOnUnsafeXtrabackupPassword pins the option-file
// guard: xtrabackup ignores MYSQL_PWD, so its password goes through a
// [client] option file — and a password that cannot be represented safely
// there must fail fast before any file is created, never silently produce a
// corrupt or injectable option file.
func TestRunFullBackupFailsFastOnUnsafeXtrabackupPassword(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &Config{BackupDir: tmpDir, BackupBin: "xtrabackup", Password: `pa"ss`}

	origID := backupIDGenerator
	backupIDGenerator = func(prefix string) string { return prefix + "seamtest" }
	defer func() { backupIDGenerator = origID }()

	err := RunFullBackup(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected RunFullBackup to fail on a password that cannot be stored in the option file")
	}
	if !strings.Contains(err.Error(), "option file") {
		t.Errorf("expected the error to mention the option file, got: %v", err)
	}

	// The rejection happens before the archive is even opened.
	matches, err := filepath.Glob(filepath.Join(tmpDir, "full_seamtest*"))
	if err != nil {
		t.Fatalf("failed to scan backup dir: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("expected no archive artifact for the rejected run, got %v", matches)
	}

	meta, err := LoadMetadata(tmpDir)
	if err != nil {
		t.Fatalf("failed to load metadata: %v", err)
	}
	if len(meta.Backups) != 1 || meta.Backups[0].Status != "failed" {
		t.Errorf("expected a single 'failed' catalog row, got %+v", meta.Backups)
	}
}

func TestSanitizeBackupArgs(t *testing.T) {
	args := []string{
		"--backup",
		"--defaults-extra-file=/tmp/secret.cnf",
		"--target-dir=/backups",
	}
	sanitized := sanitizeBackupArgs(args)
	if len(sanitized) != len(args) {
		t.Fatalf("expected len %d, got %d", len(args), len(sanitized))
	}
	if sanitized[1] != "--defaults-extra-file=[REDACTED]" {
		t.Errorf("expected redacted option file arg, got %q", sanitized[1])
	}
	if sanitized[0] != args[0] || sanitized[2] != args[2] {
		t.Errorf("expected non-sensitive args unchanged, got %v", sanitized)
	}
}

func TestNewBackupID_TimestampConsistency(t *testing.T) {
	// Pin lastIDMilli to the last millisecond of the current second (…999),
	// derived from the live clock so the monotonic-bump branch of
	// newBackupID is always exercised: time.Now() cannot exceed this value
	// within the current second, so ms is bumped to ms+1 — the first
	// millisecond of the next second.
	targetTime := time.Now().Truncate(time.Second).Add(999 * time.Millisecond)
	ms := targetTime.UnixMilli()

	lastIDMilliMu.Lock()
	lastIDMilli = ms
	lastIDMilliMu.Unlock()

	// The timestamp in the ID must reflect the bumped millisecond — the next
	// second with a 000 suffix — not the pre-bump one.
	id := newBackupID("full_")
	parts := strings.Split(strings.TrimPrefix(id, "full_"), "_")
	if len(parts) != 3 {
		t.Fatalf("unexpected id format: %s", id)
	}

	// Parse the date/time and millisecond parts
	parsedTime, err := time.ParseInLocation("20060102_150405", parts[0]+"_"+parts[1], time.Local)
	if err != nil {
		t.Fatalf("failed to parse id timestamp %s_%s: %v", parts[0], parts[1], err)
	}
	milli, err := strconv.Atoi(parts[2])
	if err != nil {
		t.Fatalf("failed to parse id millisecond %s: %v", parts[2], err)
	}
	parsedMilli := parsedTime.UnixMilli() + int64(milli)

	// The parsed time must match the monotonic ms reading exactly
	lastIDMilliMu.Lock()
	issuedMs := lastIDMilli
	lastIDMilliMu.Unlock()

	if parsedMilli != issuedMs {
		t.Errorf("parsed time milli %d != issued ms %d", parsedMilli, issuedMs)
	}
}
