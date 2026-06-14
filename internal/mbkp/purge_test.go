package mbkp

import (
	"os"
	"path/filepath"
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
			BinlogPos:  100,
		},
		{
			ID:         "inc_old",
			Type:       "incremental",
			Status:     "completed",
			StartTime:  now.Add(-8 * 24 * time.Hour).Add(-1 * time.Hour),
			EndTime:    now.Add(-8 * 24 * time.Hour),
			Path:       "inc_old.xbstream.lz4",
			BinlogFile: "mysql-bin.000003",
			BinlogPos:  200,
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
			BinlogPos:  300,
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
			BinlogPos:  50,
		},
		{
			ID:         "inc_expired",
			Type:       "incremental",
			Status:     "completed",
			StartTime:  now.Add(-11 * 24 * time.Hour).Add(-1 * time.Hour),
			EndTime:    now.Add(-11 * 24 * time.Hour),
			Path:       "inc_expired.xbstream.lz4",
			BinlogFile: "mysql-bin.000002",
			BinlogPos:  75,
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
	err = PurgeBackups(cfg, "5d", true)
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
	err = PurgeBackups(cfg, "5d", false)
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
	err = PurgeBackups(cfg, "5d", false)
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
