package mbkp

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMetadataOperations(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Initially, no backups should exist
	meta, err := LoadMetadata(tmpDir)
	if err != nil {
		t.Fatalf("LoadMetadata failed: %v", err)
	}
	if len(meta.Backups) != 0 {
		t.Errorf("expected 0 backups, got %d", len(meta.Backups))
	}

	latest, err := GetLatestBackup(tmpDir)
	if err != nil {
		t.Fatalf("GetLatestBackup failed: %v", err)
	}
	if latest != nil {
		t.Errorf("expected no latest backup, got %v", latest)
	}

	// 2. Add an in-progress full backup
	startTime := time.Now().Add(-10 * time.Minute).Truncate(time.Microsecond)
	b1 := BackupMetadata{
		ID:        "full-1",
		Type:      "full",
		Status:    "in_progress",
		StartTime: startTime,
		Path:      "full-1.xbstream.gz",
	}

	err = AddBackup(tmpDir, b1)
	if err != nil {
		t.Fatalf("AddBackup failed: %v", err)
	}

	// Should not show as latest because status is in_progress
	latest, err = GetLatestBackup(tmpDir)
	if err != nil {
		t.Fatalf("GetLatestBackup failed: %v", err)
	}
	if latest != nil {
		t.Errorf("expected no latest backup (status in_progress), got %v", latest)
	}

	// Retrieve by ID
	b, err := GetBackupByID(tmpDir, "full-1")
	if err != nil {
		t.Fatalf("GetBackupByID failed: %v", err)
	}
	if b.Status != "in_progress" {
		t.Errorf("expected status in_progress, got %s", b.Status)
	}
	if !b.StartTime.Equal(b1.StartTime.UTC()) {
		t.Errorf("expected start time %v, got %v", b1.StartTime, b.StartTime)
	}

	// 3. Complete the full backup (test upsert)
	endTime := time.Now().Add(-9 * time.Minute).Truncate(time.Microsecond)
	b1.Status = "completed"
	b1.EndTime = endTime
	b1.BinlogFile = "mysql-bin.000001"
	b1.BinlogPos = 120

	err = AddBackup(tmpDir, b1)
	if err != nil {
		t.Fatalf("AddBackup failed on upsert: %v", err)
	}

	// Now it should be the latest backup
	latest, err = GetLatestBackup(tmpDir)
	if err != nil {
		t.Fatalf("GetLatestBackup failed: %v", err)
	}
	if latest == nil || latest.ID != "full-1" {
		t.Errorf("expected latest backup to be full-1, got %v", latest)
	}

	// 4. Add an incremental backup
	b2 := BackupMetadata{
		ID:         "inc-1",
		Type:       "incremental",
		Status:     "completed",
		StartTime:  time.Now().Add(-5 * time.Minute).Truncate(time.Microsecond),
		EndTime:    time.Now().Add(-4 * time.Minute).Truncate(time.Microsecond),
		Path:       "inc-1.xbstream.gz",
		BinlogFile: "mysql-bin.000001",
		BinlogPos:  500,
		ParentID:   "full-1",
	}

	err = AddBackup(tmpDir, b2)
	if err != nil {
		t.Fatalf("AddBackup for incremental failed: %v", err)
	}

	// Now the latest should be inc-1 (because of newer end_time)
	latest, err = GetLatestBackup(tmpDir)
	if err != nil {
		t.Fatalf("GetLatestBackup failed: %v", err)
	}
	if latest == nil || latest.ID != "inc-1" {
		t.Errorf("expected latest backup to be inc-1, got %v", latest)
	}

	// 5. Test ResolveChain
	// chain of inc-1 should be [full-1, inc-1]
	chain, err := ResolveChain(tmpDir, "inc-1")
	if err != nil {
		t.Fatalf("ResolveChain failed: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("expected chain length 2, got %d", len(chain))
	}
	if chain[0].ID != "full-1" || chain[1].ID != "inc-1" {
		t.Errorf("unexpected chain elements: %v", chain)
	}

	// chain of full-1 should be [full-1]
	chainFull, err := ResolveChain(tmpDir, "full-1")
	if err != nil {
		t.Fatalf("ResolveChain failed for full-1: %v", err)
	}
	if len(chainFull) != 1 || chainFull[0].ID != "full-1" {
		t.Errorf("unexpected chain for full-1: %v", chainFull)
	}

	// 6. Test ResolveChain error cases
	// Missing backup ID
	_, err = ResolveChain(tmpDir, "non-existent")
	if err == nil {
		t.Error("expected error for non-existent backup")
	}

	// Parent ID missing or incomplete
	b3 := BackupMetadata{
		ID:        "inc-2",
		Type:      "incremental",
		Status:    "completed",
		StartTime: time.Now(),
		ParentID:  "missing-parent",
	}
	_ = AddBackup(tmpDir, b3)
	_, err = ResolveChain(tmpDir, "inc-2")
	if err == nil {
		t.Error("expected error due to missing parent")
	}

	// 7. Test GetBackupsBefore
	// b1 completed at time.Now().Add(-9 * time.Minute)
	// b2 completed at time.Now().Add(-4 * time.Minute)
	// Check before time.Now().Add(-6 * time.Minute) -> should only return full-1
	beforeTime := time.Now().Add(-6 * time.Minute)
	backups, err := GetBackupsBefore(tmpDir, beforeTime)
	if err != nil {
		t.Fatalf("GetBackupsBefore failed: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("expected 1 backup, got %d", len(backups))
	}
	if backups[0].ID != "full-1" {
		t.Errorf("expected full-1, got %s", backups[0].ID)
	}

	// Check before time.Now() -> should return both full-1 and inc-1 (in ascending order of end_time)
	backupsAll, err := GetBackupsBefore(tmpDir, time.Now())
	if err != nil {
		t.Fatalf("GetBackupsBefore failed: %v", err)
	}
	if len(backupsAll) != 2 {
		t.Errorf("expected 2 backups, got %d", len(backupsAll))
	}
	if backupsAll[0].ID != "full-1" || backupsAll[1].ID != "inc-1" {
		t.Errorf("unexpected ordering: %v", backupsAll)
	}
}

func TestCheckpointsRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()

	checkpoints := "backup_type = full-backuped\nfrom_lsn = 0\nto_lsn = 12345678\nlast_lsn = 12345678\n"

	b := BackupMetadata{
		ID:          "full-cp",
		Type:        "full",
		Status:      "in_progress",
		StartTime:   time.Now(),
		Path:        "full-cp.xbstream.gz",
		Checkpoints: checkpoints,
	}
	if err := AddBackup(tmpDir, b); err != nil {
		t.Fatalf("AddBackup failed: %v", err)
	}

	got, err := GetBackupByID(tmpDir, "full-cp")
	if err != nil {
		t.Fatalf("GetBackupByID failed: %v", err)
	}
	if got.Checkpoints != checkpoints {
		t.Errorf("expected checkpoints %q, got %q", checkpoints, got.Checkpoints)
	}

	// Upsert must update checkpoints along with the completion fields.
	updated := checkpoints + "flushed_lsn = 12345679\n"
	b.Status = "completed"
	b.EndTime = time.Now()
	b.Checkpoints = updated
	if err := AddBackup(tmpDir, b); err != nil {
		t.Fatalf("AddBackup upsert failed: %v", err)
	}
	got, err = GetBackupByID(tmpDir, "full-cp")
	if err != nil {
		t.Fatalf("GetBackupByID failed: %v", err)
	}
	if got.Checkpoints != updated {
		t.Errorf("expected updated checkpoints %q, got %q", updated, got.Checkpoints)
	}
}

func TestCheckpointsColumnMigration(t *testing.T) {
	tmpDir := t.TempDir()

	// Simulate a database created by an older mbkp version: the backups table
	// exists without the checkpoints column.
	oldDB, err := openDB(tmpDir)
	if err != nil {
		t.Fatalf("openDB failed: %v", err)
	}
	_, err = oldDB.Exec(`DROP TABLE backups`)
	if err != nil {
		t.Fatalf("failed to drop table: %v", err)
	}
	_, err = oldDB.Exec(`
		CREATE TABLE backups (
			id          TEXT PRIMARY KEY,
			type        TEXT NOT NULL,
			status      TEXT NOT NULL DEFAULT 'in_progress',
			start_time  TEXT NOT NULL,
			end_time    TEXT,
			path        TEXT NOT NULL,
			binlog_file TEXT,
			binlog_pos  INTEGER NOT NULL DEFAULT 0,
			parent_id   TEXT
		)`)
	if err != nil {
		t.Fatalf("failed to create legacy table: %v", err)
	}
	_, err = oldDB.Exec(`INSERT INTO backups (id, type, status, start_time, path) VALUES ('legacy-1', 'full', 'completed', ?, 'legacy-1.xbstream.gz')`,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("failed to insert legacy row: %v", err)
	}
	_ = oldDB.Close()

	// Opening again must add the column and keep the legacy row readable.
	got, err := GetBackupByID(tmpDir, "legacy-1")
	if err != nil {
		t.Fatalf("GetBackupByID after migration failed: %v", err)
	}
	if got.Checkpoints != "" {
		t.Errorf("expected empty checkpoints for legacy row, got %q", got.Checkpoints)
	}

	// New writes must persist checkpoints through the migrated schema.
	if err := AddBackup(tmpDir, BackupMetadata{
		ID:          "new-1",
		Type:        "full",
		Status:      "completed",
		StartTime:   time.Now(),
		EndTime:     time.Now(),
		Path:        "new-1.xbstream.gz",
		Checkpoints: "backup_type = full-backuped\n",
	}); err != nil {
		t.Fatalf("AddBackup on migrated db failed: %v", err)
	}
	got, err = GetBackupByID(tmpDir, "new-1")
	if err != nil {
		t.Fatalf("GetBackupByID failed: %v", err)
	}
	if got.Checkpoints != "backup_type = full-backuped\n" {
		t.Errorf("expected checkpoints on migrated db, got %q", got.Checkpoints)
	}

	// The catalog file must exist exactly once (no stray databases).
	if _, err := os.Stat(filepath.Join(tmpDir, dbFileName)); err != nil {
		t.Errorf("expected catalog at %s: %v", dbFileName, err)
	}
}
