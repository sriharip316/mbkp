package mbkp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseRetentionDuration(t *testing.T) {
	tests := []struct {
		input    string
		expected time.Duration
		hasError bool
	}{
		{"7d", 7 * 24 * time.Hour, false},
		{"30D", 30 * 24 * time.Hour, false},
		{"24h", 24 * time.Hour, false},
		{"1h30m", 90 * time.Minute, false},
		{"0s", 0, false},
		{"", 0, true},
		{"-7d", 0, true},
		{"-24h", 0, true},
		{"abc", 0, true},
		{"7days", 0, true},
		{"106751d", 106751 * 24 * time.Hour, false},
		{"106751D", 106751 * 24 * time.Hour, false},
		{"106752d", 0, true},
		{"106752D", 0, true},
		{"999999999999d", 0, true},
		{"99999999999999999999999999999999d", 0, true},
	}

	for _, tt := range tests {
		actual, err := ParseRetentionDuration(tt.input)
		if (err != nil) != tt.hasError {
			t.Errorf("ParseRetentionDuration(%q) error status unexpected: err=%v, expectedError=%v", tt.input, err, tt.hasError)
		}
		if err == nil && actual != tt.expected {
			t.Errorf("ParseRetentionDuration(%q) = %v, expected %v", tt.input, actual, tt.expected)
		}
	}
}

func TestParseRetentionDuration_DayOverflow(t *testing.T) {
	overflowInputs := []string{
		"106752d",
		"106752D",
		"999999999999d",
		"999999999999D",
	}

	for _, input := range overflowInputs {
		d, err := ParseRetentionDuration(input)
		if err == nil {
			t.Fatalf("ParseRetentionDuration(%q) expected error due to overflow, got nil (duration=%v)", input, d)
		}
		if d < 0 {
			t.Errorf("ParseRetentionDuration(%q) returned negative duration: %v", input, d)
		}
		if !strings.Contains(err.Error(), "out of range") || !strings.Contains(err.Error(), "max 106751 days") {
			t.Errorf("ParseRetentionDuration(%q) error %q does not mention range limit", input, err.Error())
		}
	}
}

func TestPurgeBackups(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &Config{
		BackupDir: tmpDir,
	}

	// 1. Setup metadata DB
	db, err := openDB(tmpDir)
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	_ = db.Close() // Close it so metadata helper functions can open/close it

	now := time.Now()

	// Define test backups
	// We want:
	// - full_old: completed 10 days ago (outside 5d retention, but is parent of inc_old)
	// - inc_old: completed 8 days ago (outside 5d retention, but is parent of inc_new)
	// - inc_new: completed 3 days ago (inside 5d retention) -> keeps full_old and inc_old
	// - full_expired: completed 12 days ago (outside 5d retention) -> should be purged
	// - inc_expired: completed 11 days ago (outside 5d retention) -> should be purged
	backups := []BackupMetadata{
		{
			ID:         "full_old",
			Type:       "full",
			Status:     "completed",
			StartTime:  now.Add(-10 * 24 * time.Hour).Add(-1 * time.Hour),
			EndTime:    now.Add(-10 * 24 * time.Hour),
			Path:       "full_old.xbstream.lz4",
			BinlogFile: "mysql-bin.000003",
		},
		{
			ID:         "inc_old",
			Type:       "incremental",
			Status:     "completed",
			StartTime:  now.Add(-8 * 24 * time.Hour).Add(-1 * time.Hour),
			EndTime:    now.Add(-8 * 24 * time.Hour),
			Path:       "inc_old.xbstream.lz4",
			BinlogFile: "mysql-bin.000003",
			ParentID:   "full_old",
		},
		{
			ID:         "inc_new",
			Type:       "incremental",
			Status:     "completed",
			StartTime:  now.Add(-3 * 24 * time.Hour).Add(-1 * time.Hour),
			EndTime:    now.Add(-3 * 24 * time.Hour),
			Path:       "inc_new.xbstream.lz4",
			BinlogFile: "mysql-bin.000004",
			ParentID:   "inc_old",
		},
		{
			ID:         "full_expired",
			Type:       "full",
			Status:     "completed",
			StartTime:  now.Add(-12 * 24 * time.Hour).Add(-1 * time.Hour),
			EndTime:    now.Add(-12 * 24 * time.Hour),
			Path:       "full_expired.xbstream.lz4",
			BinlogFile: "mysql-bin.000001",
		},
		{
			ID:         "inc_expired",
			Type:       "incremental",
			Status:     "completed",
			StartTime:  now.Add(-11 * 24 * time.Hour).Add(-1 * time.Hour),
			EndTime:    now.Add(-11 * 24 * time.Hour),
			Path:       "inc_expired.xbstream.lz4",
			BinlogFile: "mysql-bin.000002",
			ParentID:   "full_expired",
		},
	}

	// Create physical placeholder files and write metadata
	for _, b := range backups {
		filePath := filepath.Join(tmpDir, b.Path)
		err := os.WriteFile(filePath, []byte("placeholder"), 0644)
		if err != nil {
			t.Fatalf("failed to write archive placeholder for %s: %v", b.ID, err)
		}
		err = AddBackup(tmpDir, b)
		if err != nil {
			t.Fatalf("failed to add backup metadata for %s: %v", b.ID, err)
		}
	}

	// Create mock archived binlogs in binlogs/ subdirectory
	binlogsDir := filepath.Join(tmpDir, "binlogs")
	err = os.MkdirAll(binlogsDir, 0755)
	if err != nil {
		t.Fatalf("failed to create binlogs dir: %v", err)
	}

	binlogs := []struct {
		name    string
		ageDays int
	}{
		{"mysql-bin.000001", 10}, // expired and < oldest kept backup binlog file (mysql-bin.000003) -> purge
		{"mysql-bin.000002", 8},  // expired and < oldest kept backup binlog file (mysql-bin.000003) -> purge
		{"mysql-bin.000003", 6},  // expired but == oldest kept backup binlog file -> keep
		{"mysql-bin.000004", 3},  // within retention window and >= oldest kept -> keep
	}

	for _, bl := range binlogs {
		blPath := filepath.Join(binlogsDir, bl.name)
		err := os.WriteFile(blPath, []byte("binlog-data"), 0644)
		if err != nil {
			t.Fatalf("failed to write binlog: %v", err)
		}
		// Set mod time in the past
		modTime := now.Add(-time.Duration(bl.ageDays) * 24 * time.Hour)
		err = os.Chtimes(blPath, modTime, modTime)
		if err != nil {
			t.Fatalf("failed to set binlog mod time: %v", err)
		}
	}

	// 2. Perform a dry-run purge first and verify nothing is actually deleted
	err = PurgeBackups(context.Background(), cfg, "5d", true)
	if err != nil {
		t.Fatalf("dry-run purge failed: %v", err)
	}

	// Verify all files still exist
	for _, b := range backups {
		filePath := filepath.Join(tmpDir, b.Path)
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			t.Errorf("archive file %s was deleted during dry-run", b.ID)
		}
		meta, err := GetBackupByID(tmpDir, b.ID)
		if err != nil || meta == nil {
			t.Errorf("metadata for %s was deleted during dry-run", b.ID)
		}
	}
	for _, bl := range binlogs {
		blPath := filepath.Join(binlogsDir, bl.name)
		if _, err := os.Stat(blPath); os.IsNotExist(err) {
			t.Errorf("binlog file %s was deleted during dry-run", bl.name)
		}
	}

	// 3. Perform the actual purge with 5 days retention
	err = PurgeBackups(context.Background(), cfg, "5d", false)
	if err != nil {
		t.Fatalf("actual purge failed: %v", err)
	}

	// Verify kept backups
	keptIDs := []string{"full_old", "inc_old", "inc_new"}
	for _, id := range keptIDs {
		// Find original backup metadata to get path
		var path string
		for _, ob := range backups {
			if ob.ID == id {
				path = ob.Path
				break
			}
		}
		filePath := filepath.Join(tmpDir, path)
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			t.Errorf("expected kept archive file %s to exist on disk, but it was deleted", id)
		}
		meta, err := GetBackupByID(tmpDir, id)
		if err != nil || meta == nil {
			t.Errorf("expected kept metadata for backup %s to exist in SQLite, but it was deleted", id)
		}
	}

	// Verify purged backups
	purgedIDs := []string{"full_expired", "inc_expired"}
	for _, id := range purgedIDs {
		var path string
		for _, ob := range backups {
			if ob.ID == id {
				path = ob.Path
				break
			}
		}
		filePath := filepath.Join(tmpDir, path)
		if _, err := os.Stat(filePath); err == nil {
			t.Errorf("expected expired archive file %s to be deleted from disk, but it still exists", id)
		}
		_, err := GetBackupByID(tmpDir, id)
		if err == nil {
			t.Errorf("expected expired metadata for backup %s to be deleted from SQLite, but it still exists", id)
		}
	}

	// Verify binlogs pruning
	// mysql-bin.000001: deleted
	// mysql-bin.000002: deleted
	// mysql-bin.000003: kept (oldest kept backup binlog file)
	// mysql-bin.000004: kept (newer)
	binlogTests := []struct {
		name string
		kept bool
	}{
		{"mysql-bin.000001", false},
		{"mysql-bin.000002", false},
		{"mysql-bin.000003", true},
		{"mysql-bin.000004", true},
	}

	for _, bt := range binlogTests {
		blPath := filepath.Join(binlogsDir, bt.name)
		_, err := os.Stat(blPath)
		exists := !os.IsNotExist(err)
		if exists != bt.kept {
			t.Errorf("binlog %s kept status mismatch: expected kept=%v, actual exists=%v", bt.name, bt.kept, exists)
		}
	}

	// 4. Test external deletion scenario
	// Manually delete full_old.xbstream.lz4 from the disk.
	// This breaks the lineage of inc_old and inc_new.
	fullOldPath := filepath.Join(tmpDir, "full_old.xbstream.lz4")
	if err := os.Remove(fullOldPath); err != nil {
		t.Fatalf("failed to delete full_old archive: %v", err)
	}

	// Run PurgeBackups again.
	// - full_old metadata will be removed immediately during external deletion scan.
	// - inc_old and inc_new will fail lineage checks because their ancestor full_old is missing.
	// - As a result, all of them will be purged.
	err = PurgeBackups(context.Background(), cfg, "5d", false)
	if err != nil {
		t.Fatalf("purge after external deletion failed: %v", err)
	}

	// Verify all metadata records are cleared from SQLite
	metadataList, err := LoadMetadata(tmpDir)
	if err != nil {
		t.Fatalf("failed to load metadata: %v", err)
	}
	if len(metadataList.Backups) != 0 {
		t.Errorf("expected SQLite metadata to be fully empty after broken chain purge, but got %d items", len(metadataList.Backups))
	}

	// Verify archive files are deleted
	incOldPath := filepath.Join(tmpDir, "inc_old.xbstream.lz4")
	if _, err := os.Stat(incOldPath); err == nil {
		t.Errorf("inc_old archive was not purged after broken chain dependency analysis")
	}
	incNewPath := filepath.Join(tmpDir, "inc_new.xbstream.lz4")
	if _, err := os.Stat(incNewPath); err == nil {
		t.Errorf("inc_new archive was not purged after broken chain dependency analysis")
	}
}

func TestPurgeBackups_OldestKeptBackupNoBinlog(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &Config{
		BackupDir: tmpDir,
	}

	// Create a completed backup within retention (kept), but with NO BinlogFile recorded
	now := time.Now()
	b := BackupMetadata{
		ID:         "full_kept_no_binlog",
		Type:       "full",
		Status:     "completed",
		StartTime:  now.Add(-1 * time.Hour),
		EndTime:    now.Add(-50 * time.Minute),
		Path:       "full_kept.xbstream.lz4",
		BinlogFile: "", // empty binlog file: unknown boundary!
	}
	if err := AddBackup(tmpDir, b); err != nil {
		t.Fatalf("AddBackup failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, b.Path), []byte("archive data"), 0644); err != nil {
		t.Fatalf("write archive failed: %v", err)
	}

	// Create binlogs directory with expired binlogs
	binlogsDir := filepath.Join(tmpDir, "binlogs")
	if err := os.MkdirAll(binlogsDir, 0755); err != nil {
		t.Fatalf("mkdir binlogs failed: %v", err)
	}

	binlogFile := filepath.Join(binlogsDir, "mysql-bin.000001.lz4")
	if err := os.WriteFile(binlogFile, []byte("binlog data"), 0644); err != nil {
		t.Fatalf("write binlog failed: %v", err)
	}
	// Make binlog older than retention cutoff (e.g., 5 days old)
	expiredTime := now.Add(-5 * 24 * time.Hour)
	if err := os.Chtimes(binlogFile, expiredTime, expiredTime); err != nil {
		t.Fatalf("chtimes failed: %v", err)
	}

	// Purge with 2-day retention
	if err := PurgeBackups(context.Background(), cfg, "2d", false); err != nil {
		t.Fatalf("PurgeBackups failed: %v", err)
	}

	// The binlog MUST be retained because oldestKeptBackup has an unknown binlog boundary
	if _, err := os.Stat(binlogFile); os.IsNotExist(err) {
		t.Errorf("expected expired binlog to be retained when oldest kept backup has no BinlogFile, but it was deleted")
	}
}

func TestPurgeBackupsCanceledContext(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &Config{BackupDir: tmpDir}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := PurgeBackups(ctx, cfg, "7d", false)
	if err == nil {
		t.Fatal("expected PurgeBackups with canceled context to fail")
	}
}

func TestResolveChainInMemory_CycleDetection(t *testing.T) {
	now := time.Now()
	backupMap := map[string]BackupMetadata{
		"inc-a": {
			ID:        "inc-a",
			Type:      "incremental",
			Status:    "completed",
			StartTime: now.Add(-2 * time.Hour),
			ParentID:  "inc-b",
		},
		"inc-b": {
			ID:        "inc-b",
			Type:      "incremental",
			Status:    "completed",
			StartTime: now.Add(-1 * time.Hour),
			ParentID:  "inc-a",
		},
	}

	_, err := resolveChainInMemory(backupMap, "inc-a")
	if err == nil {
		t.Fatal("expected resolveChainInMemory to detect cycle, got nil")
	}
	if !strings.Contains(err.Error(), "cycle detected in parent chain") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestPurgeBackupsSweepsStaleTempArtifacts verifies the purge's sweep of
// temporary artifacts left behind by crashed or killed runs (SIGKILL skips
// the in-process defers that normally remove them).
func TestPurgeBackupsSweepsStaleTempArtifacts(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &Config{BackupDir: tmpDir}

	now := time.Now()
	stale := now.Add(-2 * 24 * time.Hour) // older than staleTempMaxAge

	binlogsDir := filepath.Join(tmpDir, "binlogs")
	if err := os.MkdirAll(binlogsDir, 0755); err != nil {
		t.Fatalf("failed to create binlogs dir: %v", err)
	}

	mkdir := func(path string, modTime time.Time) {
		t.Helper()
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatalf("failed to create %s: %v", path, err)
		}
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatalf("failed to set mod time on %s: %v", path, err)
		}
	}
	writeFile := func(path string, modTime time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
			t.Fatalf("failed to create %s: %v", path, err)
		}
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatalf("failed to set mod time on %s: %v", path, err)
		}
	}

	// Stale leftovers from crashed runs — all must be swept.
	stalePaths := []string{
		filepath.Join(tmpDir, "lsn_tmp_full_crashed"),
		filepath.Join(tmpDir, "target_tmp_inc_crashed"),
		filepath.Join(tmpDir, "prepare_full_crashed"),
		filepath.Join(tmpDir, "pitr_binlogs_tmp"),
		filepath.Join(binlogsDir, "mysql-bin.000001.lz4.part"),
	}
	// Fresh artifacts (within staleTempMaxAge) and unrelated files — must
	// survive every purge.
	keptPaths := []string{
		filepath.Join(tmpDir, "prepare_recent"), // e.g. restore --prepare-only output
		filepath.Join(tmpDir, "full_recent.xbstream.gz"),
		filepath.Join(binlogsDir, "mysql-bin.000002.lz4"),
	}

	mkdir(stalePaths[0], stale)
	mkdir(stalePaths[1], stale)
	mkdir(stalePaths[2], stale)
	mkdir(stalePaths[3], stale)
	writeFile(stalePaths[4], stale)
	mkdir(keptPaths[0], now)
	writeFile(keptPaths[1], now)
	writeFile(keptPaths[2], now)

	// Dry run: nothing may be removed.
	if err := PurgeBackups(context.Background(), cfg, "7d", true); err != nil {
		t.Fatalf("dry-run purge failed: %v", err)
	}
	for _, p := range append(append([]string{}, stalePaths...), keptPaths...) {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed during dry-run purge", p)
		}
	}

	// Real purge: stale leftovers swept, fresh artifacts and unrelated
	// files intact.
	if err := PurgeBackups(context.Background(), cfg, "7d", false); err != nil {
		t.Fatalf("purge failed: %v", err)
	}
	for _, p := range stalePaths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected stale leftover %s to be swept, but it still exists", p)
		}
	}
	for _, p := range keptPaths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to survive the purge, but it was removed", p)
		}
	}
}

// TestPurgeBackupsSkipsRowsWithEscapingPath plants a catalog row whose Path
// resolves outside the backup directory: purge must refuse to touch both the
// file and the metadata of that row, while legitimate expired rows around it
// purge normally (the corrupt row must not abort the whole purge).
func TestPurgeBackupsSkipsRowsWithEscapingPath(t *testing.T) {
	tmpDir := t.TempDir()
	// The backup dir is a subdirectory so the "escaping" archive can live as a
	// sibling that is still inside tmpDir and cleaned up by t.TempDir.
	backupDir := filepath.Join(tmpDir, "backups")
	cfg := &Config{BackupDir: backupDir}
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		t.Fatalf("failed to create backup dir: %v", err)
	}

	expired := time.Now().Add(-30 * 24 * time.Hour) // far outside the 7d window
	rows := []BackupMetadata{
		{
			ID:        "full_evil",
			Type:      "full",
			Status:    "completed",
			StartTime: expired,
			EndTime:   expired,
			Path:      "../evil.xbstream.gz", // resolves outside the backup dir
		},
		{
			ID:        "full_good",
			Type:      "full",
			Status:    "completed",
			StartTime: expired,
			EndTime:   expired,
			Path:      "full_good.xbstream.gz",
		},
	}
	for _, b := range rows {
		if err := AddBackup(backupDir, b); err != nil {
			t.Fatalf("failed to seed catalog: %v", err)
		}
	}

	escapeFile := filepath.Join(tmpDir, "evil.xbstream.gz")
	if err := os.WriteFile(escapeFile, []byte("x"), 0644); err != nil {
		t.Fatalf("failed to create escape file: %v", err)
	}
	goodFile := filepath.Join(backupDir, "full_good.xbstream.gz")
	if err := os.WriteFile(goodFile, []byte("x"), 0644); err != nil {
		t.Fatalf("failed to create good archive: %v", err)
	}

	if err := PurgeBackups(context.Background(), cfg, "7d", false); err != nil {
		t.Fatalf("PurgeBackups failed: %v", err)
	}

	// The escaping row: neither its file nor its metadata may be touched.
	if _, err := os.Stat(escapeFile); err != nil {
		t.Errorf("file outside the backup directory must not be deleted (stat error: %v)", err)
	}
	if _, err := GetBackupByID(backupDir, "full_evil"); err != nil {
		t.Errorf("metadata of the escaping row must be retained: %v", err)
	}

	// The legitimate expired row purges normally.
	if _, err := os.Stat(goodFile); !os.IsNotExist(err) {
		t.Error("expected the legitimate expired archive to be deleted")
	}
	if _, err := GetBackupByID(backupDir, "full_good"); err == nil {
		t.Error("expected metadata of the legitimate expired row to be deleted")
	}
}
