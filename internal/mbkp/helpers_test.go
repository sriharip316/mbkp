package mbkp

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		bytes    int64
		expected string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1048576, "1.0 MiB"},
		{1073741824, "1.0 GiB"},
		{1099511627776, "1.0 TiB"},
	}

	for _, tt := range tests {
		actual := humanBytes(tt.bytes)
		if actual != tt.expected {
			t.Errorf("humanBytes(%d): expected %q, got %q", tt.bytes, tt.expected, actual)
		}
	}
}

func TestIsBinlogFile(t *testing.T) {
	tests := []struct {
		filename string
		expected bool
	}{
		{"mysql-bin.000001", true},
		{"binlog.000123", true},
		{"binlog.abc", false},
		{"binlog.00012a", false},
		{"binlog.", false},
		{"binlog", false},
		{".000001", true},
	}

	for _, tt := range tests {
		actual := isBinlogFile(tt.filename)
		if actual != tt.expected {
			t.Errorf("isBinlogFile(%q): expected %v, got %v", tt.filename, tt.expected, actual)
		}
	}
}

func TestGetBinlogFilesToApply(t *testing.T) {
	tmpDir := t.TempDir()

	// Write mock binlog files
	binlogs := []string{
		"mysql-bin.000001",
		"mysql-bin.000002",
		"mysql-bin.000003",
		"mysql-bin.abc", // should be ignored
		"mysql-bin.000004",
	}

	for _, b := range binlogs {
		err := os.WriteFile(filepath.Join(tmpDir, b), []byte("mock-data"), 0644)
		if err != nil {
			t.Fatalf("failed to write mock binlog: %v", err)
		}
	}

	// 1. Start from mysql-bin.000002
	result, err := getBinlogFilesToApply(tmpDir, "mysql-bin.000002")
	if err != nil {
		t.Fatalf("getBinlogFilesToApply failed: %v", err)
	}

	expected := []string{
		filepath.Join(tmpDir, "mysql-bin.000002"),
		filepath.Join(tmpDir, "mysql-bin.000003"),
		filepath.Join(tmpDir, "mysql-bin.000004"),
	}

	if !reflect.DeepEqual(result, expected) {
		t.Errorf("expected %v, got %v", expected, result)
	}

	// 2. Start from mysql-bin.000005 (greater than any present)
	resultEmpty, err := getBinlogFilesToApply(tmpDir, "mysql-bin.000005")
	if err != nil {
		t.Fatalf("getBinlogFilesToApply failed: %v", err)
	}
	if len(resultEmpty) != 0 {
		t.Errorf("expected empty slice, got %v", resultEmpty)
	}
}

func TestBackupHelpers(t *testing.T) {
	// Test compressorForArchive
	compLZ4 := compressorForArchive("backup.xbstream.lz4")
	if compLZ4.Name != "lz4" {
		t.Errorf("expected lz4, got %s", compLZ4.Name)
	}

	compGzip := compressorForArchive("backup.xbstream.gz")
	if compGzip.Name != "gzip" {
		t.Errorf("expected gzip, got %s", compGzip.Name)
	}

	compDefault := compressorForArchive("backup.xbstream.unknown")
	if compDefault.Name != "gzip" {
		t.Errorf("expected fallback to gzip, got %s", compDefault.Name)
	}

	// Test archivePath
	p := archivePath("/backups", "full_123", compressorLZ4)
	expectedPath := filepath.Join("/backups", "full_123.xbstream.lz4")
	if p != expectedPath {
		t.Errorf("expected path %q, got %q", expectedPath, p)
	}

	// Test sharedLsnDir
	l := sharedLsnDir("/backups")
	expectedL := filepath.Join("/backups", "lsn")
	if l != expectedL {
		t.Errorf("expected lsn dir %q, got %q", expectedL, l)
	}
}

func TestParseBinlogInfoFromDir(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Missing file error
	_, _, err := parseBinlogInfoFromDir(tmpDir)
	if err == nil {
		t.Error("expected error when xtrabackup_binlog_info is missing")
	}

	// 2. Write valid content
	infoContent := "mysql-bin.000003\t154235\t\n"
	err = os.WriteFile(filepath.Join(tmpDir, "xtrabackup_binlog_info"), []byte(infoContent), 0644)
	if err != nil {
		t.Fatalf("failed to write xtrabackup_binlog_info: %v", err)
	}

	file, pos, err := parseBinlogInfoFromDir(tmpDir)
	if err != nil {
		t.Fatalf("parseBinlogInfoFromDir failed: %v", err)
	}
	if file != "mysql-bin.000003" {
		t.Errorf("expected file mysql-bin.000003, got %s", file)
	}
	if pos != 154235 {
		t.Errorf("expected position 154235, got %d", pos)
	}

	// 3. Write invalid position format
	err = os.WriteFile(filepath.Join(tmpDir, "xtrabackup_binlog_info"), []byte("mysql-bin.000003\tabc\n"), 0644)
	if err != nil {
		t.Fatalf("failed to write xtrabackup_binlog_info: %v", err)
	}
	_, _, err = parseBinlogInfoFromDir(tmpDir)
	if err == nil {
		t.Error("expected error for invalid position format")
	}

	// 4. Write empty file
	err = os.WriteFile(filepath.Join(tmpDir, "xtrabackup_binlog_info"), []byte(""), 0644)
	if err != nil {
		t.Fatalf("failed to write xtrabackup_binlog_info: %v", err)
	}
	_, _, err = parseBinlogInfoFromDir(tmpDir)
	if err == nil {
		t.Error("expected error for empty file")
	}

	// 5. Write unexpected content format (less than 2 fields)
	err = os.WriteFile(filepath.Join(tmpDir, "xtrabackup_binlog_info"), []byte("onlyonefield\n"), 0644)
	if err != nil {
		t.Fatalf("failed to write xtrabackup_binlog_info: %v", err)
	}
	_, _, err = parseBinlogInfoFromDir(tmpDir)
	if err == nil {
		t.Error("expected error for unexpected content format (1 field)")
	}
}

func TestParseBinlogInfoFastPath(t *testing.T) {
	tmpDir := t.TempDir()

	// Create shared lsn dir and write xtrabackup_binlog_info
	lsnDir := sharedLsnDir(tmpDir)
	if err := os.MkdirAll(lsnDir, 0755); err != nil {
		t.Fatalf("failed to create lsn dir: %v", err)
	}

	infoContent := "mysql-bin.000045\t987654\t\n"
	err := os.WriteFile(filepath.Join(lsnDir, "xtrabackup_binlog_info"), []byte(infoContent), 0644)
	if err != nil {
		t.Fatalf("failed to write xtrabackup_binlog_info: %v", err)
	}

	cfg := &Config{BackupDir: tmpDir, StreamBin: "mbstream"}
	file, pos, err := parseBinlogInfo(cfg, "backup-id", "dummy-archive")
	if err != nil {
		t.Fatalf("parseBinlogInfo failed: %v", err)
	}

	if file != "mysql-bin.000045" {
		t.Errorf("expected mysql-bin.000045, got %s", file)
	}
	if pos != 987654 {
		t.Errorf("expected 987654, got %d", pos)
	}
}

func TestCopyFile(t *testing.T) {
	tmpDir := t.TempDir()
	src := filepath.Join(tmpDir, "src.txt")
	dst := filepath.Join(tmpDir, "dst.txt")

	err := os.WriteFile(src, []byte("hello binlog copy"), 0644)
	if err != nil {
		t.Fatalf("failed to write src: %v", err)
	}

	err = copyFile(src, dst)
	if err != nil {
		t.Fatalf("copyFile failed: %v", err)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("failed to read dst: %v", err)
	}

	if string(data) != "hello binlog copy" {
		t.Errorf("expected 'hello binlog copy', got %q", string(data))
	}

	// Test copyFile errors
	err = copyFile("non-existent-source.txt", "dst.txt")
	if err == nil {
		t.Error("expected error when copying a non-existent source file")
	}

	err = copyFile(src, filepath.Join(tmpDir, "non-existent-dir", "dst.txt"))
	if err == nil {
		t.Error("expected error when destination directory does not exist")
	}
}

func TestOpenDBErrors(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "blocking_file")
	err := os.WriteFile(filePath, []byte(""), 0644)
	if err != nil {
		t.Fatalf("failed to write blocking file: %v", err)
	}

	// This path should fail during MkdirAll because filePath is a file, not a directory
	badDir := filepath.Join(filePath, "subdir")
	_, err = openDB(badDir)
	if err == nil {
		t.Error("expected error when openDB is called with a bad directory path")
	}
}

func TestCopyFileAndDir(t *testing.T) {
	tmpDir := t.TempDir()

	srcDir := filepath.Join(tmpDir, "src")
	dstDir := filepath.Join(tmpDir, "dst")

	err := os.Mkdir(srcDir, 0755)
	if err != nil {
		t.Fatalf("failed to create src dir: %v", err)
	}

	// Create sub-directory and files
	subSrcDir := filepath.Join(srcDir, "subdir")
	err = os.Mkdir(subSrcDir, 0755)
	if err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}

	f1Path := filepath.Join(srcDir, "file1.txt")
	err = os.WriteFile(f1Path, []byte("hello world"), 0644)
	if err != nil {
		t.Fatalf("failed to write file1: %v", err)
	}

	f2Path := filepath.Join(subSrcDir, "file2.txt")
	err = os.WriteFile(f2Path, []byte("sub-hello"), 0755)
	if err != nil {
		t.Fatalf("failed to write file2: %v", err)
	}

	// Test copyDir
	err = copyDir(srcDir, dstDir)
	if err != nil {
		t.Fatalf("copyDir failed: %v", err)
	}

	// Verify copies
	f1Copy := filepath.Join(dstDir, "file1.txt")
	f1Data, err := os.ReadFile(f1Copy)
	if err != nil {
		t.Fatalf("failed to read file1 copy: %v", err)
	}
	if string(f1Data) != "hello world" {
		t.Errorf("expected 'hello world', got %q", string(f1Data))
	}

	f2Copy := filepath.Join(dstDir, "subdir", "file2.txt")
	f2Data, err := os.ReadFile(f2Copy)
	if err != nil {
		t.Fatalf("failed to read file2 copy: %v", err)
	}
	if string(f2Data) != "sub-hello" {
		t.Errorf("expected 'sub-hello', got %q", string(f2Data))
	}
}

func TestArchiveSize(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Empty relative path
	if s := archiveSize(tmpDir, ""); s != "-" {
		t.Errorf("expected '-', got %q", s)
	}

	// 2. Non-existent file
	if s := archiveSize(tmpDir, "does-not-exist"); s != "-" {
		t.Errorf("expected '-', got %q", s)
	}

	// 3. Real file
	filename := "testfile.xbstream.gz"
	filePath := filepath.Join(tmpDir, filename)
	data := []byte("hello world") // 11 bytes
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	expectedSize := "11 B"
	if s := archiveSize(tmpDir, filename); s != expectedSize {
		t.Errorf("expected %q, got %q", expectedSize, s)
	}
}

func TestTargetIDError(t *testing.T) {
	origErr := os.ErrNotExist
	err1 := targetIDError("", origErr)
	if err1 == nil || err1.Error() == origErr.Error() {
		t.Errorf("expected a custom message when backupID is empty, got %v", err1)
	}

	err2 := targetIDError("some-id", origErr)
	if err2 != origErr {
		t.Errorf("expected original error when backupID is provided, got %v", err2)
	}
}

func TestDetectCompressor(t *testing.T) {
	c := detectCompressor()
	if c.Name != "lz4" && c.Name != "gzip" {
		t.Errorf("expected compressor name to be lz4 or gzip, got %s", c.Name)
	}
}
