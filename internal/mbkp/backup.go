package mbkp

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Compressor describes a compression tool and the arguments needed to compress
// (stdin → stdout) and decompress (file → stdout).
type Compressor struct {
	Name           string
	Ext            string   // archive file extension, e.g. ".xbstream.lz4"
	CompressArgs   []string // args passed when compressing: tool <args> < stdin > stdout
	DecompressArgs []string // args passed when decompressing: tool <args> <file>  (file appended at call time)
}

var (
	compressorLZ4 = Compressor{
		Name:           "lz4",
		Ext:            ".xbstream.lz4",
		CompressArgs:   []string{"-c", "-"}, // lz4 -c - : compress stdin → stdout
		DecompressArgs: []string{"-dc"},     // lz4 -dc <file> : decompress file → stdout
	}
	compressorGzip = Compressor{
		Name:           "gzip",
		Ext:            ".xbstream.gz",
		CompressArgs:   []string{"-c"},  // gzip -c : compress stdin → stdout
		DecompressArgs: []string{"-dc"}, // gzip -dc <file> : decompress file → stdout
	}
)

// detectCompressor returns the best available compression tool, preferring lz4
// (faster, lighter CPU) over gzip (universally available).
func detectCompressor() Compressor {
	if _, err := exec.LookPath("lz4"); err == nil {
		slog.Info("Compression: lz4 selected")
		return compressorLZ4
	}
	slog.Info("Compression: lz4 not found, falling back to gzip")
	return compressorGzip
}

// compressorForArchive infers the decompressor from an archive's file extension.
func compressorForArchive(archive string) Compressor {
	if strings.HasSuffix(archive, ".lz4") {
		return compressorLZ4
	}
	return compressorGzip
}

// archivePath returns the full path for a backup archive given its ID and compressor.
func archivePath(backupDir, backupID string, comp Compressor) string {
	return filepath.Join(backupDir, backupID+comp.Ext)
}

// sharedLsnDir returns the single shared lsn directory for the backup store.
// It always holds the latest LSN and is overwritten by every backup via --extra-lsndir.
// Incremental backups use it as --incremental-basedir.
func sharedLsnDir(backupDir string) string {
	return filepath.Join(backupDir, "lsn")
}

// extractArchive decompresses and extracts an archive into destDir.
// The decompressor is inferred automatically from the archive's file extension.
// It runs: <decompressor> -dc <src> | <streamBin> -x -C <destDir>
// streamBin is either "mbstream" (MariaDB) or "xbstream" (Percona/MySQL).
func extractArchive(streamBin, src, destDir string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("failed to create extract directory %s: %w", destDir, err)
	}

	comp := compressorForArchive(src)
	decompArgs := append(comp.DecompressArgs, src)
	cmdDecomp := exec.Command(comp.Name, decompArgs...)
	cmdStream := exec.Command(streamBin, "-x", "-C", destDir)

	pipe, err := cmdDecomp.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create decompressor stdout pipe: %w", err)
	}
	cmdStream.Stdin = pipe
	cmdDecomp.Stderr = os.Stderr
	cmdStream.Stderr = os.Stderr

	slog.Info("Running decompress+extract pipeline",
		"compressor", comp.Name, "stream_bin", streamBin, "args", decompArgs, "dest_dir", destDir)

	if err := cmdDecomp.Start(); err != nil {
		return fmt.Errorf("failed to start %s: %w", comp.Name, err)
	}
	if err := cmdStream.Start(); err != nil {
		_ = cmdDecomp.Process.Kill()
		return fmt.Errorf("failed to start %s: %w", streamBin, err)
	}

	decompErr := cmdDecomp.Wait()
	streamErr := cmdStream.Wait()

	if decompErr != nil {
		return fmt.Errorf("%s exited with error: %w", comp.Name, decompErr)
	}
	if streamErr != nil {
		return fmt.Errorf("%s exited with error: %w", streamBin, streamErr)
	}

	return nil
}

// parseBinlogInfoFromDir reads binlog coordinates from a backup directory.
// It first tries xtrabackup_binlog_info (mariabackup / MariaDB 10.x), then
// falls back to mariadb_backup_info (mariadb-backup / MariaDB 11.x).
func parseBinlogInfoFromDir(dir string) (string, int64, error) {
	// Legacy file: one line "<filename>\t<position>" (MariaDB 10.x)
	if f, p, err := parseLegacyBinlogInfo(filepath.Join(dir, "xtrabackup_binlog_info")); err == nil {
		return f, p, nil
	}
	// New file: key=value format (MariaDB 11.x / mariadb-backup)
	return parseMariaDBBackupInfo(filepath.Join(dir, "mariadb_backup_info"))
}

// parseLegacyBinlogInfo parses xtrabackup_binlog_info produced by mariabackup (MariaDB 10.x).
// File format: a single line "<binlog_filename>\t<position>" (whitespace-separated).
func parseLegacyBinlogInfo(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	if scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			filename := fields[0]
			pos, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return "", 0, fmt.Errorf("invalid binlog position %q in xtrabackup_binlog_info: %w", fields[1], err)
			}
			return filename, pos, nil
		}
		return "", 0, fmt.Errorf("unexpected content in xtrabackup_binlog_info: %q", line)
	}
	if err := scanner.Err(); err != nil {
		return "", 0, fmt.Errorf("error reading xtrabackup_binlog_info: %w", err)
	}
	return "", 0, fmt.Errorf("xtrabackup_binlog_info is empty")
}

// parseMariaDBBackupInfo parses mariadb_backup_info produced by mariadb-backup (MariaDB 11.x).
// The binlog position is encoded in a line of the form:
//
//	binlog_pos = filename 'binlog.000002', position '859', GTID of the last change '0-1-3'
func parseMariaDBBackupInfo(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("failed to open mariadb_backup_info in %s: %w", filepath.Dir(path), err)
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "binlog_pos") {
			continue
		}
		eqIdx := strings.Index(line, "=")
		if eqIdx < 0 {
			continue
		}
		value := strings.TrimSpace(line[eqIdx+1:])
		// Extract filename from first single-quoted token
		fn, rest, ok := extractSingleQuoted(value)
		if !ok {
			return "", 0, fmt.Errorf("could not parse filename from mariadb_backup_info binlog_pos: %q", line)
		}
		// Extract position from second single-quoted token
		posStr, _, ok := extractSingleQuoted(rest)
		if !ok {
			return "", 0, fmt.Errorf("could not parse position from mariadb_backup_info binlog_pos: %q", line)
		}
		pos, err := strconv.ParseInt(posStr, 10, 64)
		if err != nil {
			return "", 0, fmt.Errorf("invalid binlog position %q in mariadb_backup_info: %w", posStr, err)
		}
		return fn, pos, nil
	}
	if err := scanner.Err(); err != nil {
		return "", 0, fmt.Errorf("error reading mariadb_backup_info: %w", err)
	}
	return "", 0, fmt.Errorf("binlog_pos line not found in mariadb_backup_info")
}

// extractSingleQuoted extracts the content of the first single-quoted substring in s.
// Returns (content, remainder_after_closing_quote, true) on success.
func extractSingleQuoted(s string) (string, string, bool) {
	start := strings.Index(s, "'")
	if start < 0 {
		return "", s, false
	}
	end := strings.Index(s[start+1:], "'")
	if end < 0 {
		return "", s, false
	}
	content := s[start+1 : start+1+end]
	rest := s[start+1+end+1:]
	return content, rest, true
}

// parseBinlogInfo resolves binlog coordinates for a backup.
// It tries the shared lsn dir first (fast path, no extraction).
// If the binlog info file is absent there it falls back to extracting the archive.
func parseBinlogInfo(cfg *Config, backupID, archiveFile string) (string, int64, error) {
	lsnDir := sharedLsnDir(cfg.BackupDir)

	// Fast path: read directly from the shared lsn dir
	if f, p, err := parseBinlogInfoFromDir(lsnDir); err == nil {
		return f, p, nil
	}

	// Slow path: binlog info file not in lsn dir — extract from archive
	slog.Info("binlog info not in lsn dir; extracting from archive", "backup_id", backupID)
	tmpDir := filepath.Join(cfg.BackupDir, "binloginfo_tmp_"+backupID)
	if err := os.RemoveAll(tmpDir); err != nil {
		return "", 0, fmt.Errorf("failed to clean binlog info tmp dir: %w", err)
	}
	if err := extractArchive(cfg.StreamBin, archiveFile, tmpDir); err != nil {
		return "", 0, fmt.Errorf("failed to extract archive for binlog info: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			slog.Warn("failed to remove binlog info tmp dir", "tmp_dir", tmpDir, "error", err)
		}
	}()
	return parseBinlogInfoFromDir(tmpDir)
}

// streamBackup runs: <backupBin> <mariabackupArgs> | <comp> <compressArgs> > <archive>
func streamBackup(cfg *Config, archive string, comp Compressor, mariabackupArgs []string) error {
	outFile, err := os.Create(archive)
	if err != nil {
		return fmt.Errorf("failed to create archive file %s: %w", archive, err)
	}

	var tempCnfFile string
	if cfg.Password != "" && cfg.BackupBin == "xtrabackup" {
		tmpFile, err := os.CreateTemp("", "mbkp-xtrabackup-*.cnf")
		if err != nil {
			_ = outFile.Close()
			return fmt.Errorf("failed to create temporary config file: %w", err)
		}
		tempCnfFile = tmpFile.Name()
		defer func() {
			_ = os.Remove(tempCnfFile)
		}()

		content := fmt.Sprintf("[client]\npassword=\"%s\"\n", cfg.Password)
		if _, err := tmpFile.WriteString(content); err != nil {
			_ = tmpFile.Close()
			_ = outFile.Close()
			return fmt.Errorf("failed to write temporary config file: %w", err)
		}
		_ = tmpFile.Close()

		mariabackupArgs = append([]string{"--defaults-extra-file=" + tempCnfFile}, mariabackupArgs...)
	}

	cmdMariabackup := exec.Command(cfg.BackupBin, mariabackupArgs...)
	if cfg.Password != "" {
		cmdMariabackup.Env = append(os.Environ(), "MYSQL_PWD="+cfg.Password)
	}
	cmdMariabackup.Stderr = os.Stderr

	cmdCompress := exec.Command(comp.Name, comp.CompressArgs...)
	pipe, err := cmdMariabackup.StdoutPipe()
	if err != nil {
		_ = outFile.Close()
		return fmt.Errorf("failed to create %s stdout pipe: %w", cfg.BackupBin, err)
	}
	cmdCompress.Stdin = pipe
	cmdCompress.Stdout = outFile
	cmdCompress.Stderr = os.Stderr

	slog.Info("Running backup and compression command pipeline",
		"mariabackup_bin", cfg.BackupBin, "mariabackup_args", mariabackupArgs, "compressor", comp.Name, "compress_args", comp.CompressArgs, "archive", archive)

	if err := cmdMariabackup.Start(); err != nil {
		_ = outFile.Close()
		return fmt.Errorf("failed to start %s: %w", cfg.BackupBin, err)
	}
	if err := cmdCompress.Start(); err != nil {
		_ = cmdMariabackup.Process.Kill()
		_ = outFile.Close()
		return fmt.Errorf("failed to start %s: %w", comp.Name, err)
	}

	mariabackupErr := cmdMariabackup.Wait()
	compressErr := cmdCompress.Wait()
	_ = outFile.Close()

	if mariabackupErr != nil {
		_ = os.Remove(archive)
		return fmt.Errorf("%s failed: %w", cfg.BackupBin, mariabackupErr)
	}
	if compressErr != nil {
		_ = os.Remove(archive)
		return fmt.Errorf("%s failed: %w", comp.Name, compressErr)
	}
	return nil
}

func RunFullBackup(cfg *Config) error {
	comp := detectCompressor()

	timestamp := time.Now().Format("20060102_150405")
	backupID := "full_" + timestamp
	archive := archivePath(cfg.BackupDir, backupID, comp)
	lsnDir := sharedLsnDir(cfg.BackupDir)

	slog.Info("Starting full backup", "id", backupID, "archive", archive)

	if err := os.MkdirAll(cfg.BackupDir, 0755); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}

	meta := BackupMetadata{
		ID:        backupID,
		Type:      "full",
		Status:    "in_progress",
		StartTime: time.Now(),
		Path:      backupID + comp.Ext,
	}
	if err := AddBackup(cfg.BackupDir, meta); err != nil {
		return fmt.Errorf("failed to update metadata to in_progress: %w", err)
	}

	targetDir := filepath.Join(cfg.BackupDir, "target_tmp_"+backupID)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("failed to create temporary target directory: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(targetDir)
	}()

	args := []string{
		"--backup",
		"--stream=xbstream",
		"--target-dir=" + targetDir,
		"--extra-lsndir=" + lsnDir,
	}
	args = append(args, cfg.GetCommonArgs()...)

	if err := streamBackup(cfg, archive, comp, args); err != nil {
		meta.Status = "failed"
		meta.EndTime = time.Now()
		_ = AddBackup(cfg.BackupDir, meta)
		return err
	}

	binlogFile, binlogPos, err := parseBinlogInfo(cfg, backupID, archive)
	if err != nil {
		slog.Warn("failed to parse binlog coordinates", "error", err)
	}

	meta.Status = "completed"
	meta.EndTime = time.Now()
	meta.BinlogFile = binlogFile
	meta.BinlogPos = binlogPos

	if err := AddBackup(cfg.BackupDir, meta); err != nil {
		return fmt.Errorf("failed to finalize backup metadata: %w", err)
	}

	slog.Info("Full backup completed successfully",
		"id", backupID, "archive", archive, "binlog_file", binlogFile, "binlog_pos", binlogPos)
	return nil
}

func RunIncrementalBackup(cfg *Config, parentID string) error {
	var parentBackup *BackupMetadata
	var err error

	if parentID != "" {
		parentBackup, err = GetBackupByID(cfg.BackupDir, parentID)
		if err != nil {
			return fmt.Errorf("specified parent backup not found: %w", err)
		}
	} else {
		parentBackup, err = GetLatestBackup(cfg.BackupDir)
		if err != nil {
			return fmt.Errorf("failed to find latest backup to use as base: %w", err)
		}
		if parentBackup == nil {
			return fmt.Errorf("no existing completed backup found to use as base. Please run a full backup first")
		}
	}

	comp := detectCompressor()

	timestamp := time.Now().Format("20060102_150405")
	backupID := "inc_" + timestamp
	archive := archivePath(cfg.BackupDir, backupID, comp)
	lsnDir := sharedLsnDir(cfg.BackupDir)

	slog.Info("Starting incremental backup",
		"id", backupID, "parent_id", parentBackup.ID, "compressor", comp.Name)

	if err := os.MkdirAll(cfg.BackupDir, 0755); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}

	meta := BackupMetadata{
		ID:        backupID,
		Type:      "incremental",
		Status:    "in_progress",
		StartTime: time.Now(),
		Path:      backupID + comp.Ext,
		ParentID:  parentBackup.ID,
	}
	if err := AddBackup(cfg.BackupDir, meta); err != nil {
		return fmt.Errorf("failed to update metadata to in_progress: %w", err)
	}

	targetDir := filepath.Join(cfg.BackupDir, "target_tmp_"+backupID)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("failed to create temporary target directory: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(targetDir)
	}()

	// --incremental-basedir points to the shared lsn dir (latest xtrabackup_checkpoints).
	// --extra-lsndir overwrites the same dir with this backup's LSN.
	args := []string{
		"--backup",
		"--stream=xbstream",
		"--target-dir=" + targetDir,
		"--incremental-basedir=" + lsnDir,
		"--extra-lsndir=" + lsnDir,
	}
	args = append(args, cfg.GetCommonArgs()...)

	if err := streamBackup(cfg, archive, comp, args); err != nil {
		meta.Status = "failed"
		meta.EndTime = time.Now()
		_ = AddBackup(cfg.BackupDir, meta)
		return err
	}

	binlogFile, binlogPos, err := parseBinlogInfo(cfg, backupID, archive)
	if err != nil {
		slog.Warn("failed to parse binlog coordinates", "error", err)
	}

	meta.Status = "completed"
	meta.EndTime = time.Now()
	meta.BinlogFile = binlogFile
	meta.BinlogPos = binlogPos

	if err := AddBackup(cfg.BackupDir, meta); err != nil {
		return fmt.Errorf("failed to finalize backup metadata: %w", err)
	}

	slog.Info("Incremental backup completed successfully",
		"id", backupID, "archive", archive, "binlog_file", binlogFile, "binlog_pos", binlogPos)
	return nil
}
