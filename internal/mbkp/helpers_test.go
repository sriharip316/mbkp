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
		{"binlog.000001.lz4", true},
		{"binlog.000001.gz", true},
		{"binlog.000001.lz4.part", false}, // partial archive, not a usable binlog
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
}

func TestReadCheckpointsFile(t *testing.T) {
	tmpDir := t.TempDir()

	checkpointsContent := "backup_type = full-backuped\nfrom_lsn = 0\nto_lsn = 12345678\n"

	// 1. Missing file error
	_, err := readCheckpointsFile(tmpDir)
	if err == nil {
		t.Error("expected error when no checkpoints file is present")
	}

	// 2. Legacy filename (mariabackup 10.x / xtrabackup)
	err = os.WriteFile(filepath.Join(tmpDir, "xtrabackup_checkpoints"), []byte(checkpointsContent), 0644)
	if err != nil {
		t.Fatalf("failed to write xtrabackup_checkpoints: %v", err)
	}
	content, err := readCheckpointsFile(tmpDir)
	if err != nil {
		t.Fatalf("readCheckpointsFile failed: %v", err)
	}
	if content != checkpointsContent {
		t.Errorf("expected %q, got %q", checkpointsContent, content)
	}

	// 3. MariaDB 11.1+ filename (mariadb-backup)
	tmpDir2 := t.TempDir()
	err = os.WriteFile(filepath.Join(tmpDir2, "mariadb_backup_checkpoints"), []byte(checkpointsContent), 0644)
	if err != nil {
		t.Fatalf("failed to write mariadb_backup_checkpoints: %v", err)
	}
	content, err = readCheckpointsFile(tmpDir2)
	if err != nil {
		t.Fatalf("readCheckpointsFile failed: %v", err)
	}
	if content != checkpointsContent {
		t.Errorf("expected %q, got %q", checkpointsContent, content)
	}
}

func TestWriteCheckpointsDir(t *testing.T) {
	tmpDir := t.TempDir()
	dir := filepath.Join(tmpDir, "incbase")

	checkpointsContent := "backup_type = full-backuped\nfrom_lsn = 0\nto_lsn = 12345678\n"
	if err := writeCheckpointsDir(dir, checkpointsContent); err != nil {
		t.Fatalf("writeCheckpointsDir failed: %v", err)
	}

	// Both supported filenames must be present with the same content so any
	// tool version can consume the directory as --incremental-basedir.
	for _, name := range checkpointsFileNames {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("expected %s to be written: %v", name, err)
		}
		if string(data) != checkpointsContent {
			t.Errorf("expected %q in %s, got %q", checkpointsContent, name, string(data))
		}
	}

	// Round-trip: readCheckpointsFile must recover the content.
	content, err := readCheckpointsFile(dir)
	if err != nil {
		t.Fatalf("readCheckpointsFile failed: %v", err)
	}
	if content != checkpointsContent {
		t.Errorf("round-trip mismatch: expected %q, got %q", checkpointsContent, content)
	}
}

func TestReadBinlogInfoFile(t *testing.T) {
	// Real content captured from the info files the supported tools write into
	// the --extra-lsndir output.
	xtrabackupInfo := "uuid = 53852a5d\nbinlog_pos = filename 'binlog.000002', position '325', GTID of the last change ''\nformat = file\n"
	mariadbBackupInfo := "tool_name = mariadb-backup\nbinlog_pos = filename 'binlog.000002', position '325', GTID of the last change ''\nformat = file\n"

	// 1. Missing file error
	tmpDir := t.TempDir()
	_, err := readBinlogInfoFile(tmpDir)
	if err == nil {
		t.Error("expected error when no binlog info file is present")
	}

	// 2. Dedicated file (xtrabackup_binlog_info, tab-separated)
	err = os.WriteFile(filepath.Join(tmpDir, "xtrabackup_binlog_info"), []byte("binlog.000002\t325\t\n"), 0644)
	if err != nil {
		t.Fatalf("failed to write xtrabackup_binlog_info: %v", err)
	}
	content, err := readBinlogInfoFile(tmpDir)
	if err != nil {
		t.Fatalf("readBinlogInfoFile failed: %v", err)
	}
	if content != "binlog.000002\t325\t\n" {
		t.Errorf("unexpected content: %q", content)
	}

	// 3. Key=value info file (mariabackup 10.x / xtrabackup write this to the lsn dir)
	tmpDir2 := t.TempDir()
	err = os.WriteFile(filepath.Join(tmpDir2, "xtrabackup_info"), []byte(xtrabackupInfo), 0644)
	if err != nil {
		t.Fatalf("failed to write xtrabackup_info: %v", err)
	}
	content, err = readBinlogInfoFile(tmpDir2)
	if err != nil {
		t.Fatalf("readBinlogInfoFile failed: %v", err)
	}
	if content != xtrabackupInfo {
		t.Errorf("unexpected content: %q", content)
	}

	// 4. MariaDB 11.1+ filename (mariadb-backup)
	tmpDir3 := t.TempDir()
	err = os.WriteFile(filepath.Join(tmpDir3, "mariadb_backup_info"), []byte(mariadbBackupInfo), 0644)
	if err != nil {
		t.Fatalf("failed to write mariadb_backup_info: %v", err)
	}
	content, err = readBinlogInfoFile(tmpDir3)
	if err != nil {
		t.Fatalf("readBinlogInfoFile failed: %v", err)
	}
	if content != mariadbBackupInfo {
		t.Errorf("unexpected content: %q", content)
	}
}

func TestParseBinlogInfoContent(t *testing.T) {
	// 1. Dedicated-file format (xtrabackup_binlog_info): "<filename>\t<pos>"
	file, pos, err := parseBinlogInfoContent("mysql-bin.000003\t154235\t\n")
	if err != nil {
		t.Fatalf("parseBinlogInfoContent failed: %v", err)
	}
	if file != "mysql-bin.000003" || pos != 154235 {
		t.Errorf("expected mysql-bin.000003/154235, got %s/%d", file, pos)
	}

	// 2. Key=value format with GTID suffix (MariaDB info files)
	file, pos, err = parseBinlogInfoContent("uuid = x\nbinlog_pos = filename 'binlog.000002', position '325', GTID of the last change '0-1-3'\nformat = file\n")
	if err != nil {
		t.Fatalf("parseBinlogInfoContent failed: %v", err)
	}
	if file != "binlog.000002" || pos != 325 {
		t.Errorf("expected binlog.000002/325, got %s/%d", file, pos)
	}

	// 3. Key=value format without GTID suffix (Percona xtrabackup_info)
	file, pos, err = parseBinlogInfoContent("tool_name = xtrabackup\nbinlog_pos = filename 'binlog.000004', position '158'\n")
	if err != nil {
		t.Fatalf("parseBinlogInfoContent failed: %v", err)
	}
	if file != "binlog.000004" || pos != 158 {
		t.Errorf("expected binlog.000004/158, got %s/%d", file, pos)
	}

	// 4. Invalid position in dedicated-file format
	if _, _, err := parseBinlogInfoContent("mysql-bin.000003\tabc\n"); err == nil {
		t.Error("expected error for invalid position format")
	}

	// 5. Empty content
	if _, _, err := parseBinlogInfoContent(""); err == nil {
		t.Error("expected error for empty content")
	}

	// 6. Dedicated-file format with less than 2 fields
	if _, _, err := parseBinlogInfoContent("onlyonefield\n"); err == nil {
		t.Error("expected error for unexpected content format (1 field)")
	}

	// 7. Key=value content without a binlog_pos line
	if _, _, err := parseBinlogInfoContent("uuid = x\nformat = file\n"); err == nil {
		t.Error("expected error when binlog_pos line is missing")
	}

	// 8. Malformed binlog_pos line (no quoted values)
	if _, _, err := parseBinlogInfoContent("binlog_pos = not quoted at all\n"); err == nil {
		t.Error("expected error for malformed binlog_pos line")
	}
}

func TestCaptureLsnInfo(t *testing.T) {
	// A realistic per-run lsn dir as mariabackup 10.x leaves it after a
	// streamed backup: checkpoints + key=value info file with binlog_pos.
	lsnDir := t.TempDir()
	checkpoints := "backup_type = full-backuped\nfrom_lsn = 0\nto_lsn = 12345678\n"
	info := "uuid = x\nbinlog_pos = filename 'binlog.000002', position '325', GTID of the last change ''\n"
	if err := os.WriteFile(filepath.Join(lsnDir, "xtrabackup_checkpoints"), []byte(checkpoints), 0644); err != nil {
		t.Fatalf("failed to write checkpoints: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lsnDir, "xtrabackup_info"), []byte(info), 0644); err != nil {
		t.Fatalf("failed to write xtrabackup_info: %v", err)
	}

	gotCheckpoints, gotInfo, gotFile, gotPos := captureLsnInfo(lsnDir)
	if gotCheckpoints != checkpoints {
		t.Errorf("expected checkpoints %q, got %q", checkpoints, gotCheckpoints)
	}
	if gotInfo != info {
		t.Errorf("expected info %q, got %q", info, gotInfo)
	}
	if gotFile != "binlog.000002" || gotPos != 325 {
		t.Errorf("expected binlog.000002/325, got %s/%d", gotFile, gotPos)
	}

	// Empty lsn dir: capture must degrade to empty values instead of failing.
	gotCheckpoints, gotInfo, gotFile, gotPos = captureLsnInfo(t.TempDir())
	if gotCheckpoints != "" || gotInfo != "" || gotFile != "" || gotPos != 0 {
		t.Errorf("expected empty capture results, got %q/%q/%s/%d",
			gotCheckpoints, gotInfo, gotFile, gotPos)
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
