package mbkp

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func captureStdout(f func() error) (string, error) {
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	os.Stdout = w

	errChan := make(chan error, 1)
	outChan := make(chan string)

	go func() {
		var buf bytes.Buffer
		_, err := io.Copy(&buf, r)
		if err != nil {
			errChan <- err
			return
		}
		outChan <- buf.String()
	}()

	errFunc := f()
	_ = w.Close()
	os.Stdout = old

	select {
	case err := <-errChan:
		return "", err
	case out := <-outChan:
		return out, errFunc
	}
}

func TestListBackups(t *testing.T) {
	tmpDir := t.TempDir()

	b1 := BackupMetadata{
		ID:         "full-123",
		Type:       "full",
		Status:     "completed",
		StartTime:  time.Now().Add(-1 * time.Hour),
		EndTime:    time.Now().Add(-59 * time.Minute),
		Path:       "full-123.xbstream.gz",
		BinlogFile: "mysql-bin.000001",
		BinlogPos:  120,
	}
	err := AddBackup(tmpDir, b1)
	if err != nil {
		t.Fatalf("AddBackup failed: %v", err)
	}

	b2 := BackupMetadata{
		ID:        "failed-456",
		Type:      "incremental",
		Status:    "failed",
		StartTime: time.Now().Add(-30 * time.Minute),
		EndTime:   time.Now().Add(-29 * time.Minute),
		Path:      "failed-456.xbstream.gz",
		ParentID:  "full-123",
	}
	err = AddBackup(tmpDir, b2)
	if err != nil {
		t.Fatalf("AddBackup failed for b2: %v", err)
	}

	b3 := BackupMetadata{
		ID:        "progress-789",
		Type:      "full",
		Status:    "in_progress",
		StartTime: time.Now(),
		Path:      "", // empty path
	}
	err = AddBackup(tmpDir, b3)
	if err != nil {
		t.Fatalf("AddBackup failed for b3: %v", err)
	}

	cfg := &Config{
		BackupDir: tmpDir,
	}

	// JSON output
	outJSON, err := captureStdout(func() error {
		return ListBackups(cfg, "json")
	})
	if err != nil {
		t.Fatalf("ListBackups JSON failed: %v", err)
	}
	if !strings.Contains(outJSON, `"id": "full-123"`) {
		t.Errorf("expected JSON to contain 'full-123', got %q", outJSON)
	}

	// Table output
	outTable, err := captureStdout(func() error {
		return ListBackups(cfg, "table")
	})
	if err != nil {
		t.Fatalf("ListBackups table failed: %v", err)
	}
	if !strings.Contains(outTable, "full-123") {
		t.Errorf("expected table to contain 'full-123', got %q", outTable)
	}

	// Empty table
	tmpDir2 := t.TempDir()
	cfg2 := &Config{
		BackupDir: tmpDir2,
	}
	outEmpty, err := captureStdout(func() error {
		return ListBackups(cfg2, "table")
	})
	if err != nil {
		t.Fatalf("ListBackups empty failed: %v", err)
	}
	if !strings.Contains(outEmpty, "No backups found.") {
		t.Errorf("expected empty table message, got %q", outEmpty)
	}
}
