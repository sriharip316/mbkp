package mbkp

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestScanBinaryLogs_StandardColumns(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory sqlite db: %v", err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`CREATE TABLE binlogs (Log_name TEXT, File_size INTEGER)`)
	if err != nil {
		t.Fatalf("failed to create table: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO binlogs (Log_name, File_size) VALUES
		('mysql-bin.000001', 512),
		('mysql-bin.000002', 1024)
	`)
	if err != nil {
		t.Fatalf("failed to insert rows: %v", err)
	}

	rows, err := db.Query("SELECT Log_name, File_size FROM binlogs ORDER BY Log_name ASC")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer func() { _ = rows.Close() }()

	logs, err := scanBinaryLogs(rows)
	if err != nil {
		t.Fatalf("scanBinaryLogs failed unexpectedly: %v", err)
	}

	if len(logs) != 2 {
		t.Fatalf("expected 2 binlogs, got %d", len(logs))
	}
	if logs[0].LogName != "mysql-bin.000001" {
		t.Errorf("unexpected logs[0]: %+v", logs[0])
	}
	if logs[1].LogName != "mysql-bin.000002" {
		t.Errorf("unexpected logs[1]: %+v", logs[1])
	}
}

func TestScanBinaryLogs_EncryptedColumn(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory sqlite db: %v", err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`CREATE TABLE binlogs (Log_name TEXT, File_size INTEGER, Encrypted TEXT)`)
	if err != nil {
		t.Fatalf("failed to create table: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO binlogs (Log_name, File_size, Encrypted) VALUES
		('binlog.000001', 2048, 'Y'),
		('binlog.000002', 4096, 'N')
	`)
	if err != nil {
		t.Fatalf("failed to insert rows: %v", err)
	}

	rows, err := db.Query("SELECT Log_name, File_size, Encrypted FROM binlogs ORDER BY Log_name ASC")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer func() { _ = rows.Close() }()

	logs, err := scanBinaryLogs(rows)
	if err != nil {
		t.Fatalf("scanBinaryLogs failed unexpectedly: %v", err)
	}

	if len(logs) != 2 {
		t.Fatalf("expected 2 binlogs, got %d", len(logs))
	}
	if logs[0].LogName != "binlog.000001" {
		t.Errorf("unexpected logs[0]: %+v", logs[0])
	}
	if logs[1].LogName != "binlog.000002" {
		t.Errorf("unexpected logs[1]: %+v", logs[1])
	}
}

func TestScanBinaryLogs_IterationError(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory sqlite db: %v", err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`CREATE TABLE binlogs (Log_name TEXT, File_size INTEGER)`)
	if err != nil {
		t.Fatalf("failed to create table: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO binlogs (Log_name, File_size) VALUES
		('binlog.000001', 512),
		('binlog.000002', 1024),
		('binlog.000003', 2048)
	`)
	if err != nil {
		t.Fatalf("failed to insert rows: %v", err)
	}

	// Trigger an error during SQLite step iteration on the second row using malformed JSON in json_extract.
	// This simulates a mid-iteration failure (e.g. network disconnect during SHOW BINARY LOGS streaming).
	rows, err := db.Query(`
		SELECT
			Log_name,
			CASE WHEN Log_name = 'binlog.000002' THEN json_extract('{', '$.x') ELSE File_size END AS File_size
		FROM binlogs
	`)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer func() { _ = rows.Close() }()

	_, err = scanBinaryLogs(rows)
	if err == nil {
		t.Fatal("expected error from scanBinaryLogs during iteration error, got nil")
	}

	if !strings.Contains(err.Error(), "error iterating binary log rows") {
		t.Errorf("expected error message to contain %q, got: %v", "error iterating binary log rows", err)
	}
}

func TestScanBinaryLogs_ScanError(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory sqlite db: %v", err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`CREATE TABLE binlogs (Log_name TEXT, File_size TEXT)`)
	if err != nil {
		t.Fatalf("failed to create table: %v", err)
	}
	_, err = db.Exec(`INSERT INTO binlogs (Log_name, File_size) VALUES ('binlog.000001', 'not-an-integer')`)
	if err != nil {
		t.Fatalf("failed to insert row: %v", err)
	}

	rows, err := db.Query("SELECT Log_name, File_size FROM binlogs")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer func() { _ = rows.Close() }()

	_, err = scanBinaryLogs(rows)
	if err == nil {
		t.Fatal("expected error from scanBinaryLogs due to non-integer size, got nil")
	}

	if !strings.Contains(err.Error(), "failed to scan binary log row") {
		t.Errorf("expected error message to contain %q, got: %v", "failed to scan binary log row", err)
	}
}

func TestScanBinaryLogs_Empty(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory sqlite db: %v", err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`CREATE TABLE binlogs (Log_name TEXT, File_size INTEGER)`)
	if err != nil {
		t.Fatalf("failed to create table: %v", err)
	}

	rows, err := db.Query("SELECT Log_name, File_size FROM binlogs")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer func() { _ = rows.Close() }()

	logs, err := scanBinaryLogs(rows)
	if err != nil {
		t.Fatalf("scanBinaryLogs failed unexpectedly on empty table: %v", err)
	}

	if len(logs) != 0 {
		t.Errorf("expected empty binlogs slice, got %d elements", len(logs))
	}
}
