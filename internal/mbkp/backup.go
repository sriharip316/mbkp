package mbkp

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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
func extractArchive(ctx context.Context, streamBin, src, destDir string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("failed to create extract directory %s: %w", destDir, err)
	}

	comp := compressorForArchive(src)
	decompArgs := append(slices.Clone(comp.DecompressArgs), src)
	cmdDecomp := exec.CommandContext(ctx, comp.Name, decompArgs...)
	cmdStream := exec.CommandContext(ctx, streamBin, "-x", "-C", destDir)

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
		_ = cmdDecomp.Wait()
		return fmt.Errorf("failed to start %s: %w", streamBin, err)
	}

	decompErr := cmdDecomp.Wait()
	streamErr := cmdStream.Wait()

	if err := ctx.Err(); err != nil {
		return err
	}

	if decompErr != nil {
		return fmt.Errorf("%s exited with error: %w", comp.Name, decompErr)
	}
	if streamErr != nil {
		return fmt.Errorf("%s exited with error: %w", streamBin, streamErr)
	}

	return nil
}

// parseBinlogInfoContent parses the binlog filename and GTID set from the raw
// content of a captured binlog info file. Two formats occur across the
// supported tools:
//
//   - the dedicated xtrabackup_binlog_info file (mariabackup 10.x, xtrabackup):
//     a single tab-separated line "<binlog_filename>\t<position>\t<gtid_set>",
//     where the position field is no longer used, and
//
//   - the key=value info files (xtrabackup_info, mariadb_backup_info), whose
//     coordinates are encoded in a line of the form:
//
//     binlog_pos = filename 'binlog.000002', position '859', GTID of the last change '0-1-3'
//
// The GTID set is the replay boundary for PITR; the filename anchors binlog
// retention and PITR archive-completeness checks.
func parseBinlogInfoContent(content string) (string, string, error) {
	// Dedicated-file format first: "<filename>\t<position>\t<gtid>"
	if f, g, err := parseLegacyBinlogInfoContent(content); err == nil {
		return f, g, nil
	}
	// Key=value format: scan for the binlog_pos line
	return parseMariaDBBackupInfoContent(content)
}

// parseLegacyBinlogInfoContent parses the content of xtrabackup_binlog_info
// produced by mariabackup (MariaDB 10.x) and xtrabackup.
// Format: a single tab-separated line "<binlog_filename>\t<position>\t<gtid_set>".
func parseLegacyBinlogInfoContent(content string) (string, string, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	if scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			// The middle field is the numeric binlog position; validating it
			// keeps this parser from matching key=value info-file lines like
			// "uuid = x", whose second field is "=".
			if _, err := strconv.ParseInt(fields[1], 10, 64); err != nil {
				return "", "", fmt.Errorf("invalid binlog position %q in xtrabackup_binlog_info: %w", fields[1], err)
			}
			filename := fields[0]
			gtid := ""
			if len(fields) >= 3 {
				gtid = fields[2]
			}
			return filename, gtid, nil
		}
		return "", "", fmt.Errorf("unexpected content in xtrabackup_binlog_info: %q", line)
	}
	if err := scanner.Err(); err != nil {
		return "", "", fmt.Errorf("error reading xtrabackup_binlog_info: %w", err)
	}
	return "", "", fmt.Errorf("xtrabackup_binlog_info is empty")
}

// parseMariaDBBackupInfoContent parses the binlog_pos line from the key=value
// info files (xtrabackup_info / mariadb_backup_info) all supported backup
// tools write into the --extra-lsndir output. The line has the form:
//
//	binlog_pos = filename 'binlog.000002', position '859', GTID of the last change '0-1-3'
//
// The third quoted value (the GTID set) may be absent when the server runs
// without GTIDs; the position field is no longer used.
func parseMariaDBBackupInfoContent(content string) (string, string, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "binlog_pos") {
			continue
		}
		_, after, ok0 := strings.Cut(line, "=")
		if !ok0 {
			continue
		}
		value := strings.TrimSpace(after)
		// Extract filename from first single-quoted token
		fn, rest, ok := extractSingleQuoted(value)
		if !ok {
			return "", "", fmt.Errorf("could not parse filename from binlog info binlog_pos: %q", line)
		}
		// Skip position (second quoted token); extract GTID from the third.
		gtid := ""
		if _, restAfterPos, ok := extractSingleQuoted(rest); ok {
			if g, _, ok := extractSingleQuoted(restAfterPos); ok {
				gtid = g
			}
		}
		return fn, gtid, nil
	}
	if err := scanner.Err(); err != nil {
		return "", "", fmt.Errorf("error reading binlog info: %w", err)
	}
	return "", "", fmt.Errorf("binlog_pos line not found in binlog info content")
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
// into the per-run --extra-lsndir output, and parses the binlog filename and
// GTID set from the captured binlog info content. The backup has already
// succeeded at this point, so capture failures are logged as warnings and
// yield empty values rather than failing the run.
func captureLsnInfo(lsnDir string) (checkpoints, binlogFile, gtid string) {
	checkpoints, err := readCheckpointsFile(lsnDir)
	if err != nil {
		slog.Warn("failed to capture checkpoints file", "error", err)
	}
	info, err := readBinlogInfoFile(lsnDir)
	if err != nil {
		slog.Warn("failed to capture binlog info file", "error", err)
	}
	if info != "" {
		binlogFile, gtid, err = parseBinlogInfoContent(info)
		if err != nil {
			slog.Warn("failed to parse binlog coordinates", "error", err)
		}
	}
	if gtid == "" {
		slog.Warn("backup has no GTID coordinates; point-in-time recovery will not be possible for this backup " +
			"(MySQL/Percona servers must run with --gtid-mode=ON --enforce-gtid-consistency=ON)")
	}
	return checkpoints, binlogFile, gtid
}

// backupID state for newBackupID: the millisecond reading of the last issued
// ID, kept strictly monotonic so rapid successive calls within one process
// (or a backwards clock step) can never repeat an ID.
var (
	lastIDMilliMu sync.Mutex
	lastIDMilli   int64
)

// newBackupID returns a backup identifier of the form
// <prefix>YYYYMMDD_HHMMSS_mmm (e.g. full_20260914_093000_482). The suffix is
// derived from the millisecond clock and kept strictly monotonic within the
// process, so IDs never repeat even when two calls land in the same
// millisecond (small databases, scripted loops, retries) or the clock steps
// backwards. Across processes, the per-directory lock serializes runs and
// insert-only catalog writes turn any residual duplicate into a loud error.
func newBackupID(prefix string) string {
	t := time.Now()
	ms := t.UnixMilli()

	lastIDMilliMu.Lock()
	if ms <= lastIDMilli {
		ms = lastIDMilli + 1
	}
	lastIDMilli = ms
	lastIDMilliMu.Unlock()

	return fmt.Sprintf("%s%s_%03d", prefix, t.Format("20060102_150405"), ms%1000)
}

// backupIDGenerator produces the IDs used by the backup entry points. It is a
// variable only so tests can pin a deterministic ID and thereby force the
// failure paths that hinge on per-run directory names; production always
// uses newBackupID.
var backupIDGenerator = newBackupID

// backupIDRe matches safe backup IDs consisting only of alphanumeric characters,
// dashes, and underscores. This protects against directory traversal and path
// injection when backup IDs are used in filesystem operations.
var backupIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// isValidBackupID reports whether id contains only characters safe for filesystem paths.
func isValidBackupID(id string) bool {
	return backupIDRe.MatchString(id)
}

// streamBackup runs: <backupBin> <mariabackupArgs> | <comp> <compressArgs> > <archive>
func streamBackup(ctx context.Context, cfg *Config, archive string, comp Compressor, mariabackupArgs []string) error {
	outFile, err := os.OpenFile(archive, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
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

	cmdMariabackup := exec.CommandContext(ctx, cfg.BackupBin, mariabackupArgs...)
	if cfg.Password != "" {
		cmdMariabackup.Env = append(os.Environ(), "MYSQL_PWD="+cfg.Password)
	}
	cmdMariabackup.Stderr = os.Stderr

	cmdCompress := exec.CommandContext(ctx, comp.Name, comp.CompressArgs...)
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
		_ = cmdMariabackup.Wait()
		_ = outFile.Close()
		return fmt.Errorf("failed to start %s: %w", comp.Name, err)
	}

	mariabackupErr := cmdMariabackup.Wait()
	compressErr := cmdCompress.Wait()
	_ = outFile.Close()

	if err := ctx.Err(); err != nil {
		_ = os.Remove(archive)
		return err
	}

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

func RunFullBackup(ctx context.Context, cfg *Config) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}

	comp := detectCompressor()

	backupID := backupIDGenerator("full_")
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
		return fmt.Errorf("failed to record in_progress metadata: %w", err)
	}
	// The row now exists in the catalog: every failure from here on —
	// including cancellation — must move it out of in_progress, or purge
	// would retain the orphaned row for the whole retention window.
	defer func() {
		if err == nil {
			return
		}
		meta.Status = "failed"
		meta.EndTime = time.Now()
		if uerr := UpdateBackup(cfg.BackupDir, meta); uerr != nil {
			slog.Error("failed to record backup failure in catalog", "id", meta.ID, "error", uerr)
		}
	}()

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

	if err := streamBackup(ctx, cfg, archive, comp, args); err != nil {
		return err
	}

	checkpoints, binlogFile, gtid := captureLsnInfo(lsnDir)

	meta.Status = "completed"
	meta.EndTime = time.Now()
	meta.BinlogFile = binlogFile
	meta.Gtid = gtid
	meta.Checkpoints = checkpoints

	if err := UpdateBackup(cfg.BackupDir, meta); err != nil {
		return fmt.Errorf("failed to finalize backup metadata: %w", err)
	}

	slog.Info("Full backup completed successfully",
		"id", backupID, "archive", archive, "binlog_file", binlogFile, "gtid", gtid)
	return nil
}

func RunIncrementalBackup(ctx context.Context, cfg *Config, parentID string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}

	var parentBackup *BackupMetadata

	if parentID != "" {
		if !isValidBackupID(parentID) {
			return fmt.Errorf("invalid parent backup ID %q", parentID)
		}
		pb, err := GetBackupByID(cfg.BackupDir, parentID)
		if err != nil {
			return fmt.Errorf("specified parent backup not found: %w", err)
		}
		parentBackup = pb
		if parentBackup.Status != "completed" {
			return fmt.Errorf("specified parent backup %s is not completed (status: %s)",
				parentBackup.ID, parentBackup.Status)
		}
	} else {
		pb, err := GetLatestBackup(cfg.BackupDir)
		if err != nil {
			return fmt.Errorf("failed to find latest backup to use as base: %w", err)
		}
		if pb == nil {
			return fmt.Errorf("no existing completed backup found to use as base. Please run a full backup first")
		}
		parentBackup = pb
	}

	// The incremental delta must be taken against the backup that parent_id
	// records, so the parent's checkpoints must be available in the catalog.
	if parentBackup.Checkpoints == "" {
		return fmt.Errorf("parent backup %s has no stored checkpoints (created by an older mbkp version, or capture failed at backup time); take a new full backup before running incremental backups",
			parentBackup.ID)
	}

	comp := detectCompressor()

	backupID := backupIDGenerator("inc_")
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
		return fmt.Errorf("failed to record in_progress metadata: %w", err)
	}
	// The row now exists in the catalog: every failure from here on —
	// including cancellation — must move it out of in_progress, or purge
	// would retain the orphaned row for the whole retention window.
	defer func() {
		if err == nil {
			return
		}
		meta.Status = "failed"
		meta.EndTime = time.Now()
		if uerr := UpdateBackup(cfg.BackupDir, meta); uerr != nil {
			slog.Error("failed to record backup failure in catalog", "id", meta.ID, "error", uerr)
		}
	}()

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

	if err := streamBackup(ctx, cfg, archive, comp, args); err != nil {
		return err
	}

	checkpoints, binlogFile, gtid := captureLsnInfo(lsnDir)

	meta.Status = "completed"
	meta.EndTime = time.Now()
	meta.BinlogFile = binlogFile
	meta.Gtid = gtid
	meta.Checkpoints = checkpoints

	if err := UpdateBackup(cfg.BackupDir, meta); err != nil {
		return fmt.Errorf("failed to finalize backup metadata: %w", err)
	}

	slog.Info("Incremental backup completed successfully",
		"id", backupID, "archive", archive, "binlog_file", binlogFile, "gtid", gtid)
	return nil
}
