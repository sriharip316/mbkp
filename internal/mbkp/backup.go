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

// checkpointsFileNames lists the filenames the supported backup tools use for
// checkpoint data: xtrabackup_checkpoints (mariabackup 10.x, xtrabackup) and
// mariadb_backup_checkpoints (mariadb-backup 11.1+, MDEV-18931).
var checkpointsFileNames = []string{"xtrabackup_checkpoints", "mariadb_backup_checkpoints"}

// binlogInfoFileNames lists the filenames the supported backup tools use for
// the info file that carries the binlog coordinates into the --extra-lsndir
// output: xtrabackup_binlog_info / xtrabackup_info (mariabackup 10.x,
// xtrabackup) and mariadb_backup_info (mariadb-backup 11.1+, MDEV-18931).
// On all supported tools the key=value info file contains a "binlog_pos" line;
// xtrabackup_binlog_info (tab-separated) is listed first in case a tool
// version copies it as well.
var binlogInfoFileNames = []string{"xtrabackup_binlog_info", "xtrabackup_info", "mariadb_backup_info"}

// readInfoFile returns the raw content of the first file in dir whose name
// matches one of candidates.
func readInfoFile(dir string, candidates []string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("failed to read lsn dir %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		for _, name := range candidates {
			if entry.Name() != name {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				return "", fmt.Errorf("failed to read %s: %w", entry.Name(), err)
			}
			return string(data), nil
		}
	}
	return "", fmt.Errorf("no info file (%s) found in %s",
		strings.Join(candidates, " or "), dir)
}

// readCheckpointsFile reads the raw content of the checkpoints file in dir,
// accepting any of the supported tool-specific filenames.
func readCheckpointsFile(dir string) (string, error) {
	return readInfoFile(dir, checkpointsFileNames)
}

// readBinlogInfoFile reads the raw content of the binlog info file in dir,
// accepting any of the supported tool-specific filenames.
func readBinlogInfoFile(dir string) (string, error) {
	return readInfoFile(dir, binlogInfoFileNames)
}

// writeCheckpointsDir materializes checkpoint content into dir under every
// supported filename so that any backup tool version can consume the directory
// as --incremental-basedir.
func writeCheckpointsDir(dir, content string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create lsn dir %s: %w", dir, err)
	}
	for _, name := range checkpointsFileNames {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to write %s: %w", name, err)
		}
	}
	return nil
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

// parseBinlogInfoContent parses binlog coordinates from the raw content of a
// captured binlog info file. Two formats occur across the supported tools:
//
//   - the dedicated xtrabackup_binlog_info file (mariabackup 10.x, xtrabackup):
//     a single line "<binlog_filename>\t<position>" (optionally followed by a
//     GTID set), and
//
//   - the key=value info files (xtrabackup_info, mariadb_backup_info), whose
//     binlog position is encoded in a line of the form:
//
//     binlog_pos = filename 'binlog.000002', position '859', GTID of the last change '0-1-3'
func parseBinlogInfoContent(content string) (string, int64, error) {
	// Dedicated-file format first: one line "<filename>\t<position>"
	if f, p, err := parseLegacyBinlogInfoContent(content); err == nil {
		return f, p, nil
	}
	// Key=value format: scan for the binlog_pos line
	return parseMariaDBBackupInfoContent(content)
}

// parseLegacyBinlogInfoContent parses the content of xtrabackup_binlog_info
// produced by mariabackup (MariaDB 10.x) and xtrabackup.
// Format: a single line "<binlog_filename>\t<position>" (whitespace-separated).
func parseLegacyBinlogInfoContent(content string) (string, int64, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
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

// parseMariaDBBackupInfoContent parses the binlog_pos line from the key=value
// info files (xtrabackup_info / mariadb_backup_info) all supported backup
// tools write into the --extra-lsndir output. The line has the form:
//
//	binlog_pos = filename 'binlog.000002', position '859', GTID of the last change '0-1-3'
func parseMariaDBBackupInfoContent(content string) (string, int64, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
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
			return "", 0, fmt.Errorf("could not parse filename from binlog info binlog_pos: %q", line)
		}
		// Extract position from second single-quoted token
		posStr, _, ok := extractSingleQuoted(rest)
		if !ok {
			return "", 0, fmt.Errorf("could not parse position from binlog info binlog_pos: %q", line)
		}
		pos, err := strconv.ParseInt(posStr, 10, 64)
		if err != nil {
			return "", 0, fmt.Errorf("invalid binlog position %q in binlog info: %w", posStr, err)
		}
		return fn, pos, nil
	}
	if err := scanner.Err(); err != nil {
		return "", 0, fmt.Errorf("error reading binlog info: %w", err)
	}
	return "", 0, fmt.Errorf("binlog_pos line not found in binlog info content")
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

// captureLsnInfo reads the checkpoints and binlog info files the backup wrote
// into the per-run --extra-lsndir output, and parses binlog coordinates from
// the captured binlog info content. The backup has already succeeded at this
// point, so capture failures are logged as warnings and yield empty values —
// the raw content is persisted in the catalog either way.
func captureLsnInfo(lsnDir string) (checkpoints, binlogInfo, binlogFile string, binlogPos int64) {
	checkpoints, err := readCheckpointsFile(lsnDir)
	if err != nil {
		slog.Warn("failed to capture checkpoints file", "error", err)
	}
	binlogInfo, err = readBinlogInfoFile(lsnDir)
	if err != nil {
		slog.Warn("failed to capture binlog info file", "error", err)
	}
	if binlogInfo != "" {
		binlogFile, binlogPos, err = parseBinlogInfoContent(binlogInfo)
		if err != nil {
			slog.Warn("failed to parse binlog coordinates", "error", err)
		}
	}
	return checkpoints, binlogInfo, binlogFile, binlogPos
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
	// Per-run temp dir receiving the --extra-lsndir output (checkpoints and
	// binlog info); its content is persisted in the catalog before cleanup.
	lsnDir := filepath.Join(cfg.BackupDir, "lsn_tmp_"+backupID)
	defer func() {
		_ = os.RemoveAll(lsnDir)
	}()

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

	checkpoints, binlogInfo, binlogFile, binlogPos := captureLsnInfo(lsnDir)

	meta.Status = "completed"
	meta.EndTime = time.Now()
	meta.BinlogFile = binlogFile
	meta.BinlogPos = binlogPos
	meta.Checkpoints = checkpoints
	meta.BinlogInfo = binlogInfo

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
		if parentBackup.Status != "completed" {
			return fmt.Errorf("specified parent backup %s is not completed (status: %s)",
				parentBackup.ID, parentBackup.Status)
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

	// The incremental delta must be taken against the backup that parent_id
	// records, so the parent's checkpoints must be available in the catalog.
	if parentBackup.Checkpoints == "" {
		return fmt.Errorf("parent backup %s has no stored checkpoints (created by an older mbkp version, or capture failed at backup time); take a new full backup before running incremental backups",
			parentBackup.ID)
	}

	comp := detectCompressor()

	timestamp := time.Now().Format("20060102_150405")
	backupID := "inc_" + timestamp
	archive := archivePath(cfg.BackupDir, backupID, comp)
	// Per-run temp dir receiving the --extra-lsndir output (checkpoints and
	// binlog info); its content is persisted in the catalog before cleanup.
	lsnDir := filepath.Join(cfg.BackupDir, "lsn_tmp_"+backupID)
	defer func() {
		_ = os.RemoveAll(lsnDir)
	}()

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

	// Materialize the parent's stored checkpoints into a temporary basedir so
	// --incremental-basedir always matches the backup that parent_id records,
	// never whatever backup happened to run last.
	incBaseDir := filepath.Join(cfg.BackupDir, "incbase_tmp_"+backupID)
	if err := os.RemoveAll(incBaseDir); err != nil {
		return fmt.Errorf("failed to clean incremental base dir: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(incBaseDir)
	}()
	if err := writeCheckpointsDir(incBaseDir, parentBackup.Checkpoints); err != nil {
		return fmt.Errorf("failed to materialize parent checkpoints: %w", err)
	}

	// --incremental-basedir points at the materialized parent checkpoints.
	// --extra-lsndir captures this backup's own checkpoints for later incrementals.
	args := []string{
		"--backup",
		"--stream=xbstream",
		"--target-dir=" + targetDir,
		"--incremental-basedir=" + incBaseDir,
		"--extra-lsndir=" + lsnDir,
	}
	args = append(args, cfg.GetCommonArgs()...)

	if err := streamBackup(cfg, archive, comp, args); err != nil {
		meta.Status = "failed"
		meta.EndTime = time.Now()
		_ = AddBackup(cfg.BackupDir, meta)
		return err
	}

	checkpoints, binlogInfo, binlogFile, binlogPos := captureLsnInfo(lsnDir)

	meta.Status = "completed"
	meta.EndTime = time.Now()
	meta.BinlogFile = binlogFile
	meta.BinlogPos = binlogPos
	meta.Checkpoints = checkpoints
	meta.BinlogInfo = binlogInfo

	if err := AddBackup(cfg.BackupDir, meta); err != nil {
		return fmt.Errorf("failed to finalize backup metadata: %w", err)
	}

	slog.Info("Incremental backup completed successfully",
		"id", backupID, "archive", archive, "binlog_file", binlogFile, "binlog_pos", binlogPos)
	return nil
}
