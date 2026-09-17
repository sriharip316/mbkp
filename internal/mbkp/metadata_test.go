package mbkp

import (
	"strings"
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

	// 3. Complete the full backup (test the in_progress → completed update)
	endTime := time.Now().Add(-9 * time.Minute).Truncate(time.Microsecond)
	b1.Status = "completed"
	b1.EndTime = endTime
	b1.BinlogFile = "mysql-bin.000001"
	b1.Gtid = "0-1-5"

	err = UpdateBackup(tmpDir, b1)
	if err != nil {
		t.Fatalf("UpdateBackup failed: %v", err)
	}

	// Now it should be the latest backup
	latest, err = GetLatestBackup(tmpDir)
	if err != nil {
		t.Fatalf("GetLatestBackup failed: %v", err)
	}
	if latest == nil || latest.ID != "full-1" {
		t.Errorf("expected latest backup to be full-1, got %v", latest)
	}
	if latest.BinlogFile != "mysql-bin.000001" || latest.Gtid != "0-1-5" {
		t.Errorf("expected binlog file/GTID mysql-bin.000001/0-1-5, got %s/%s", latest.BinlogFile, latest.Gtid)
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
		Gtid:       "0-1-9",
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

	// Update must set checkpoints along with the completion fields.
	updated := checkpoints + "flushed_lsn = 12345679\n"
	b.Status = "completed"
	b.EndTime = time.Now()
	b.Checkpoints = updated
	if err := UpdateBackup(tmpDir, b); err != nil {
		t.Fatalf("UpdateBackup failed: %v", err)
	}
	got, err = GetBackupByID(tmpDir, "full-cp")
	if err != nil {
		t.Fatalf("GetBackupByID failed: %v", err)
	}
	if got.Checkpoints != updated {
		t.Errorf("expected updated checkpoints %q, got %q", updated, got.Checkpoints)
	}
}

func TestStrictInsertAndUpdate(t *testing.T) {
	tmpDir := t.TempDir()

	b := BackupMetadata{
		ID:        "full-dup",
		Type:      "full",
		Status:    "in_progress",
		StartTime: time.Now(),
		Path:      "full-dup.xbstream.gz",
	}
	if err := AddBackup(tmpDir, b); err != nil {
		t.Fatalf("AddBackup failed: %v", err)
	}

	// Inserting the same ID again must fail loudly: a shared ID means two runs
	// would silently merge their metadata (the W2 corruption mode).
	b.Status = "completed"
	if err := AddBackup(tmpDir, b); err == nil {
		t.Error("expected duplicate-ID insert to fail, got nil")
	}

	// The failed re-insert must not have modified the original row.
	got, err := GetBackupByID(tmpDir, "full-dup")
	if err != nil {
		t.Fatalf("GetBackupByID failed: %v", err)
	}
	if got.Status != "in_progress" {
		t.Errorf("original row must be untouched by the failed insert, got status %s", got.Status)
	}

	// Updating a row that does not exist is an error, not a silent no-op.
	missing := BackupMetadata{ID: "no-such-backup", Status: "completed", EndTime: time.Now()}
	if err := UpdateBackup(tmpDir, missing); err == nil {
		t.Error("expected UpdateBackup on missing row to fail, got nil")
	}
}

func TestSameSecondTimestampOrderingAndFiltering(t *testing.T) {
	tmpDir := t.TempDir()

	// Base time at an exact second boundary (0 nanoseconds)
	t0 := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	// t1 is half a second later within the same second (500,000,000 nanoseconds)
	t1 := time.Date(2026, 9, 17, 12, 0, 0, 500_000_000, time.UTC)

	b1 := BackupMetadata{
		ID:        "backup-exact-sec",
		Type:      "full",
		Status:    "in_progress",
		StartTime: t0.Add(-5 * time.Minute),
		Path:      "b1.xbstream.gz",
	}
	if err := AddBackup(tmpDir, b1); err != nil {
		t.Fatalf("AddBackup b1 failed: %v", err)
	}
	b1.Status = "completed"
	b1.EndTime = t0
	if err := UpdateBackup(tmpDir, b1); err != nil {
		t.Fatalf("UpdateBackup b1 failed: %v", err)
	}

	b2 := BackupMetadata{
		ID:        "backup-frac-sec",
		Type:      "incremental",
		Status:    "in_progress",
		StartTime: t1.Add(-1 * time.Minute),
		Path:      "b2.xbstream.gz",
		ParentID:  "backup-exact-sec",
	}
	if err := AddBackup(tmpDir, b2); err != nil {
		t.Fatalf("AddBackup b2 failed: %v", err)
	}
	b2.Status = "completed"
	b2.EndTime = t1
	if err := UpdateBackup(tmpDir, b2); err != nil {
		t.Fatalf("UpdateBackup b2 failed: %v", err)
	}

	// 1. GetLatestBackup must return backup-frac-sec (t1 is chronologically later than t0).
	// Under the RFC3339Nano bug, "12:00:00Z" > "12:00:00.5Z" because 'Z' > '.', which would
	// return backup-exact-sec instead.
	latest, err := GetLatestBackup(tmpDir)
	if err != nil {
		t.Fatalf("GetLatestBackup failed: %v", err)
	}
	if latest == nil {
		t.Fatal("expected latest backup, got nil")
	}
	if latest.ID != "backup-frac-sec" {
		t.Errorf("GetLatestBackup returned %q, want %q", latest.ID, "backup-frac-sec")
	}

	// 2. GetBackupsBefore with cutoff in between t0 and t1:
	// Cutoff is 12:00:00.250000000Z.
	// b1 (12:00:00.000000000Z) should be included.
	// b2 (12:00:00.500000000Z) should be excluded.
	cutoff := time.Date(2026, 9, 17, 12, 0, 0, 250_000_000, time.UTC)
	before, err := GetBackupsBefore(tmpDir, cutoff)
	if err != nil {
		t.Fatalf("GetBackupsBefore failed: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("expected 1 backup before %v, got %d: %+v", cutoff, len(before), before)
	}
	if before[0].ID != "backup-exact-sec" {
		t.Errorf("GetBackupsBefore returned %q, want %q", before[0].ID, "backup-exact-sec")
	}

	// 3. GetBackupsBefore after both backups:
	afterCutoff := time.Date(2026, 9, 17, 12, 0, 1, 0, time.UTC)
	both, err := GetBackupsBefore(tmpDir, afterCutoff)
	if err != nil {
		t.Fatalf("GetBackupsBefore after both failed: %v", err)
	}
	if len(both) != 2 {
		t.Fatalf("expected 2 backups, got %d", len(both))
	}
	if both[0].ID != "backup-exact-sec" || both[1].ID != "backup-frac-sec" {
		t.Errorf("expected [backup-exact-sec, backup-frac-sec], got [%s, %s]", both[0].ID, both[1].ID)
	}
}

func TestLegacyRFC3339NanoTimestampParsing(t *testing.T) {
	tmpDir := t.TempDir()

	db, err := openDB(tmpDir)
	if err != nil {
		t.Fatalf("openDB failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Directly insert a row formatted with legacy RFC3339Nano (no trailing fractional zeros)
	legacyStart := "2026-09-17T10:00:00Z"
	legacyEnd := "2026-09-17T10:30:00Z"
	_, err = db.Exec(`
		INSERT INTO backups (id, type, status, start_time, end_time, path)
		VALUES ('legacy-1', 'full', 'completed', ?, ?, 'legacy-1.xbstream.gz')
	`, legacyStart, legacyEnd)
	if err != nil {
		t.Fatalf("direct insert failed: %v", err)
	}

	b, err := GetBackupByID(tmpDir, "legacy-1")
	if err != nil {
		t.Fatalf("GetBackupByID failed: %v", err)
	}
	wantStart := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 9, 17, 10, 30, 0, 0, time.UTC)
	if !b.StartTime.Equal(wantStart) {
		t.Errorf("StartTime = %v, want %v", b.StartTime, wantStart)
	}
	if !b.EndTime.Equal(wantEnd) {
		t.Errorf("EndTime = %v, want %v", b.EndTime, wantEnd)
	}
}

func TestScanBackupTimestampParseErrors(t *testing.T) {
	tmpDir := t.TempDir()

	db, err := openDB(tmpDir)
	if err != nil {
		t.Fatalf("openDB failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	// 1. Invalid start_time should fail with an error naming the column, invalid value, and backup ID
	_, err = db.Exec(`
		INSERT INTO backups (id, type, status, start_time, end_time, path)
		VALUES ('bad-start-1', 'full', 'completed', 'corrupted-timestamp', NULL, 'bad-start-1.xbstream.gz')
	`)
	if err != nil {
		t.Fatalf("insert bad-start-1 failed: %v", err)
	}

	_, err = GetBackupByID(tmpDir, "bad-start-1")
	if err == nil {
		t.Fatal("expected error for invalid start_time, got nil")
	}
	if !strings.Contains(err.Error(), "failed to parse start_time") ||
		!strings.Contains(err.Error(), "corrupted-timestamp") ||
		!strings.Contains(err.Error(), "bad-start-1") {
		t.Errorf("unexpected error message: %v", err)
	}

	// LoadMetadata should also fail on bad start_time
	_, err = LoadMetadata(tmpDir)
	if err == nil {
		t.Fatal("expected LoadMetadata to fail on invalid start_time, got nil")
	}

	// 2. Invalid end_time should fail with an error naming the column, invalid value, and backup ID
	_, err = db.Exec(`
		INSERT INTO backups (id, type, status, start_time, end_time, path)
		VALUES ('bad-end-1', 'full', 'completed', '2026-09-17T12:00:00Z', 'corrupted-end-time', 'bad-end-1.xbstream.gz')
	`)
	if err != nil {
		t.Fatalf("insert bad-end-1 failed: %v", err)
	}

	_, err = GetBackupByID(tmpDir, "bad-end-1")
	if err == nil {
		t.Fatal("expected error for invalid end_time, got nil")
	}
	if !strings.Contains(err.Error(), "failed to parse end_time") ||
		!strings.Contains(err.Error(), "corrupted-end-time") ||
		!strings.Contains(err.Error(), "bad-end-1") {
		t.Errorf("unexpected error message: %v", err)
	}

	// 3. NULL end_time should succeed and leave EndTime as zero value
	_, err = db.Exec(`
		INSERT INTO backups (id, type, status, start_time, end_time, path)
		VALUES ('valid-null-end', 'full', 'in_progress', '2026-09-17T12:00:00Z', NULL, 'valid.xbstream.gz')
	`)
	if err != nil {
		t.Fatalf("insert valid-null-end failed: %v", err)
	}

	b, err := GetBackupByID(tmpDir, "valid-null-end")
	if err != nil {
		t.Fatalf("GetBackupByID for valid-null-end failed: %v", err)
	}
	if !b.EndTime.IsZero() {
		t.Errorf("expected zero EndTime for NULL end_time, got %v", b.EndTime)
	}
	wantStart := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	if !b.StartTime.Equal(wantStart) {
		t.Errorf("StartTime = %v, want %v", b.StartTime, wantStart)
	}
}
