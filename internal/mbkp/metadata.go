package mbkp

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const dbFileName = "backups.db"

// BackupMetadata describes a single backup entry stored in the database.
type BackupMetadata struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`   // "full" or "incremental"
	Status     string    `json:"status"` // "in_progress", "completed", "failed"
	StartTime  time.Time `json:"start_time"`
	EndTime    time.Time `json:"end_time,omitempty"`
	Path       string    `json:"path"`        // Relative path to the archive file
	BinlogFile string    `json:"binlog_file"` // Binlog filename at backup end
	BinlogPos  int64     `json:"binlog_pos"`  // Binlog position at backup end
	ParentID   string    `json:"parent_id"`   // Empty for full, parent ID for incremental
}

// Metadata holds the full list of backups; used by the list command.
type Metadata struct {
	Backups []BackupMetadata `json:"backups"`
}

func dbPath(backupDir string) string {
	return filepath.Join(backupDir, dbFileName)
}

// openDB opens (or creates) the SQLite backup database and ensures the schema exists.
// Callers are responsible for calling db.Close().
func openDB(backupDir string) (*sql.DB, error) {
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create backup directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath(backupDir))
	if err != nil {
		return nil, fmt.Errorf("failed to open backup database: %w", err)
	}

	// WAL mode allows reads during writes; good practice even for single-writer tools.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to set WAL journal mode: %w", err)
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS backups (
			id          TEXT PRIMARY KEY,
			type        TEXT NOT NULL,
			status      TEXT NOT NULL DEFAULT 'in_progress',
			start_time  TEXT NOT NULL,
			end_time    TEXT,
			path        TEXT NOT NULL,
			binlog_file TEXT,
			binlog_pos  INTEGER NOT NULL DEFAULT 0,
			parent_id   TEXT
		)
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to create backups table: %w", err)
	}

	return db, nil
}

// scanBackup scans a single row from the backups table into a BackupMetadata value.
func scanBackup(rows *sql.Rows) (BackupMetadata, error) {
	var b BackupMetadata
	var startTimeStr string
	var endTimeStr, binlogFile, parentID sql.NullString
	var binlogPos int64

	if err := rows.Scan(
		&b.ID, &b.Type, &b.Status,
		&startTimeStr, &endTimeStr,
		&b.Path, &binlogFile, &binlogPos, &parentID,
	); err != nil {
		return b, fmt.Errorf("failed to scan backup row: %w", err)
	}

	b.StartTime, _ = time.Parse(time.RFC3339Nano, startTimeStr)
	if endTimeStr.Valid {
		b.EndTime, _ = time.Parse(time.RFC3339Nano, endTimeStr.String)
	}
	b.BinlogFile = binlogFile.String
	b.BinlogPos = binlogPos
	b.ParentID = parentID.String

	return b, nil
}

const selectBackup = `
	SELECT id, type, status, start_time, end_time, path, binlog_file, binlog_pos, parent_id
	FROM backups`

// AddBackup inserts a new backup row or updates an existing one (upsert by ID).
func AddBackup(backupDir string, backup BackupMetadata) error {
	db, err := openDB(backupDir)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	// Store nullable fields as SQL NULL when empty/zero.
	var endTime, parentID interface{}
	if !backup.EndTime.IsZero() {
		endTime = backup.EndTime.UTC().Format(time.RFC3339Nano)
	}
	if backup.ParentID != "" {
		parentID = backup.ParentID
	}

	_, err = db.Exec(`
		INSERT INTO backups (id, type, status, start_time, end_time, path, binlog_file, binlog_pos, parent_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			status      = excluded.status,
			end_time    = excluded.end_time,
			binlog_file = excluded.binlog_file,
			binlog_pos  = excluded.binlog_pos
	`,
		backup.ID, backup.Type, backup.Status,
		backup.StartTime.UTC().Format(time.RFC3339Nano),
		endTime,
		backup.Path,
		backup.BinlogFile,
		backup.BinlogPos,
		parentID,
	)
	if err != nil {
		return fmt.Errorf("failed to upsert backup %s: %w", backup.ID, err)
	}
	return nil
}

// LoadMetadata returns all backup rows ordered by start time; used by the list command.
func LoadMetadata(backupDir string) (*Metadata, error) {
	db, err := openDB(backupDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query(selectBackup + ` ORDER BY start_time ASC`)
	if err != nil {
		return nil, fmt.Errorf("failed to query backups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var backups []BackupMetadata
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		backups = append(backups, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating backup rows: %w", err)
	}

	if backups == nil {
		backups = []BackupMetadata{}
	}
	return &Metadata{Backups: backups}, nil
}

// GetLatestBackup returns the most recently completed backup, or nil if none exist.
func GetLatestBackup(backupDir string) (*BackupMetadata, error) {
	db, err := openDB(backupDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query(selectBackup + `
		WHERE status = 'completed'
		ORDER BY end_time DESC LIMIT 1`)
	if err != nil {
		return nil, fmt.Errorf("failed to query latest backup: %w", err)
	}
	defer func() { _ = rows.Close() }()

	if rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		return &b, nil
	}
	return nil, nil
}

// GetBackupByID returns the backup with the given ID, or an error if not found.
func GetBackupByID(backupDir string, id string) (*BackupMetadata, error) {
	db, err := openDB(backupDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query(selectBackup+` WHERE id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("failed to query backup by ID: %w", err)
	}
	defer func() { _ = rows.Close() }()

	if rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		return &b, nil
	}
	return nil, fmt.Errorf("backup with ID %q not found", id)
}

// ResolveChain returns the ordered backup chain (full → ... → target) needed to restore targetID.
func ResolveChain(backupDir string, targetID string) ([]BackupMetadata, error) {
	db, err := openDB(backupDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	// Walk the parent_id chain from target back to the root full backup.
	var chain []BackupMetadata
	currID := targetID

	for currID != "" {
		rows, err := db.Query(selectBackup+` WHERE id = ? AND status = 'completed'`, currID)
		if err != nil {
			return nil, fmt.Errorf("failed to query backup %q: %w", currID, err)
		}

		if !rows.Next() {
			_ = rows.Close()
			return nil, fmt.Errorf("backup ID %q in lineage chain not found or not completed", currID)
		}
		b, err := scanBackup(rows)
		_ = rows.Close()
		if err != nil {
			return nil, err
		}

		chain = append(chain, b)
		currID = b.ParentID
	}

	// Reverse: chain was built target → root; we need root → target.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}

	if len(chain) == 0 {
		return nil, fmt.Errorf("empty chain resolved for target backup ID %q", targetID)
	}
	if chain[0].Type != "full" {
		return nil, fmt.Errorf("invalid chain for target ID %q: must start with a full backup, got %q",
			targetID, chain[0].Type)
	}

	return chain, nil
}

// GetBackupsBefore returns all completed backups whose end time is before t, sorted ascending.
func GetBackupsBefore(backupDir string, t time.Time) ([]BackupMetadata, error) {
	db, err := openDB(backupDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query(selectBackup+`
		WHERE status = 'completed' AND end_time < ?
		ORDER BY end_time ASC`,
		t.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query backups before time: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var backups []BackupMetadata
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		backups = append(backups, b)
	}
	return backups, rows.Err()
}

// DeleteBackup deletes a backup record from the metadata database by its ID.
func DeleteBackup(backupDir string, id string) error {
	db, err := openDB(backupDir)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	_, err = db.Exec("DELETE FROM backups WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("failed to delete backup %s: %w", id, err)
	}
	return nil
}
