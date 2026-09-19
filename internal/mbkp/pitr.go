package mbkp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// binlogBaseName strips an optional .lz4/.gz compression suffix from a binlog filename.
// Partial archives (".part") keep their suffix so they are never mistaken for usable binlogs.
func binlogBaseName(name string) string {
	if before, ok := strings.CutSuffix(name, ".lz4"); ok {
		return before
	}
	if before, ok := strings.CutSuffix(name, ".gz"); ok {
		return before
	}
	return name
}

// isBinlogFile checks if the file is a MariaDB binary log file (has a numeric extension, optionally compressed)
func isBinlogFile(filename string) bool {
	filename = binlogBaseName(filename)
	ext := filepath.Ext(filename) // e.g. ".000001"
	if len(ext) < 2 {
		return false
	}
	// Check if all characters after the dot are digits
	for _, char := range ext[1:] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func getBinlogFilesToApply(binlogsDir string, startFile string) ([]string, error) {
	entries, err := os.ReadDir(binlogsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read binlogs directory: %w", err)
	}

	var filenames []string
	for _, entry := range entries {
		if !entry.IsDir() && isBinlogFile(entry.Name()) {
			filenames = append(filenames, entry.Name())
		}
	}

	sort.Strings(filenames)

	var filtered []string
	seenBase := make(map[string]bool)
	for _, name := range filenames {
		base := binlogBaseName(name)
		if seenBase[base] {
			// A second compression variant of a binlog already selected (e.g.
			// a .gz from an earlier run alongside a .lz4). Applying both would
			// decompress into the same output file and replay the events
			// twice; the first variant in sorted order wins.
			slog.Warn("duplicate archived variant of the same binlog; applying it once",
				"skipped", name, "binlog", base)
			continue
		}
		seenBase[base] = true
		if base >= startFile {
			filtered = append(filtered, filepath.Join(binlogsDir, name))
		}
	}

	return filtered, nil
}

const (
	// pitrConnectTimeout bounds a single readiness connect attempt so one hung
	// handshake cannot stretch the poll loop beyond its overall budget.
	pitrConnectTimeout = 10 * time.Second
	// daemonStopTimeout is how long the deferred shutdown waits after
	// SIGTERM before escalating to SIGKILL.
	daemonStopTimeout = 15 * time.Second
)

func RunPITR(ctx context.Context, cfg *Config, targetTime time.Time, datadir string, newServerUUID bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	slog.Info("Starting PITR recovery", "target_time", targetTime.Format(time.RFC3339))

	// 1. Find the latest completed backup before the target time
	backups, err := GetBackupsBefore(cfg.BackupDir, targetTime)
	if err != nil {
		return fmt.Errorf("failed to list backups: %w", err)
	}

	if len(backups) == 0 {
		return fmt.Errorf("no completed backups found that ended before target time %s", targetTime.Format(time.RFC3339))
	}

	// The backups slice is sorted by EndTime ascending, so the last element is the closest one before the target time
	baseBackup := backups[len(backups)-1]
	slog.Info("Found closest backup to restore", "id", baseBackup.ID, "type", baseBackup.Type, "end_time", baseBackup.EndTime.Format(time.RFC3339))

	// Replay is GTID-based: without a recorded GTID set the post-backup events
	// cannot be distinguished from the already-backed-up ones. Fail before the
	// expensive restore so the operator learns immediately.
	if baseBackup.Gtid == "" {
		return fmt.Errorf("base backup %s has no GTID coordinates (binlog info missing at backup time, or the server ran without GTIDs; MySQL/Percona servers need --gtid-mode=ON); point-in-time recovery is not possible for this backup", baseBackup.ID)
	}
	// The binlog filename anchors the archive-completeness check below.
	if baseBackup.BinlogFile == "" {
		return fmt.Errorf("base backup %s has no binlog filename recorded; cannot verify binlog archive completeness", baseBackup.ID)
	}

	// 2. Restore the selected backup
	slog.Info("Restoring backup", "id", baseBackup.ID)
	// We restore it directly to the datadir (prepare + copy-back). The restore
	// also re-establishes the backed-up server-uuid via auto.cnf (unless
	// newServerUUID is set), so the recovery daemon — and any server started
	// on this datadir afterwards — continues the original GTID identity.
	if err := RestoreBackup(ctx, cfg, baseBackup.ID, datadir, false, newServerUUID); err != nil {
		return fmt.Errorf("failed to restore base backup for PITR: %w", err)
	}

	// 3. Automatically start database server locally for PITR recovery
	binary, err := findServerBinary()
	if err != nil {
		return fmt.Errorf("failed to locate MariaDB/MySQL server binary: %w", err)
	}

	// If running as root, make sure the mysql user owns the datadir
	if os.Getuid() == 0 {
		slog.Info("Running as root, changing ownership of datadir to mysql...", "datadir", datadir)
		chownCmd := exec.CommandContext(ctx, "chown", "-R", "mysql:mysql", datadir)
		if err := chownCmd.Run(); err != nil {
			slog.Warn("Failed to chown datadir to mysql:mysql", "error", err)
		}
	}

	var daemonArgs []string
	daemonArgs = append(daemonArgs, "--datadir="+datadir)
	daemonArgs = append(daemonArgs, "--pid-file="+filepath.Join(datadir, "recovery.pid"))

	if os.Getuid() == 0 {
		daemonArgs = append(daemonArgs, "--user=mysql")
	}

	if cfg.Socket != "" {
		// Ensure socket directory exists
		socketDir := filepath.Dir(cfg.Socket)
		if err := os.MkdirAll(socketDir, 0755); err == nil && os.Getuid() == 0 {
			_ = exec.CommandContext(ctx, "chown", "mysql:mysql", socketDir).Run()
		}
		daemonArgs = append(daemonArgs, "--socket="+cfg.Socket)
		daemonArgs = append(daemonArgs, "--skip-networking")
	} else {
		daemonArgs = append(daemonArgs, "--port="+strconv.Itoa(cfg.Port))
		daemonArgs = append(daemonArgs, "--bind-address=127.0.0.1")
	}

	// The replay stream carries SET GTID_NEXT statements, which a
	// gtid_mode=OFF server rejects. MySQL-family servers default to OFF
	// (MariaDB always has GTIDs enabled), so enable GTID mode for the
	// temporary replay server.
	if cfg.BinlogBin == "mysqlbinlog" {
		daemonArgs = append(daemonArgs, "--gtid-mode=ON", "--enforce-gtid-consistency=ON")
	}

	_ = os.MkdirAll(cfg.BackupDir, 0755)
	logFilePath := filepath.Join(cfg.BackupDir, "pitr_mariadbd.log")
	logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to create recovery database log file: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	cmdDaemon := exec.CommandContext(ctx, binary, daemonArgs...)
	cmdDaemon.Cancel = func() error {
		return cmdDaemon.Process.Signal(syscall.SIGTERM)
	}
	cmdDaemon.WaitDelay = 10 * time.Second
	cmdDaemon.Stdout = logFile
	cmdDaemon.Stderr = logFile

	slog.Info("Starting temporary database server for recovery", "binary", binary, "args", daemonArgs, "log", logFilePath)
	if err := cmdDaemon.Start(); err != nil {
		return fmt.Errorf("failed to start temporary database server: %w", err)
	}

	// The watcher goroutine is the sole owner of cmdDaemon.Wait(): it reaps
	// the process exactly once and closes daemonExited so any number of
	// waiters (readiness poll, deferred shutdown) can observe the exit.
	// daemonErr is only read after daemonExited has closed, which gives the
	// necessary happens-before edge over the goroutine's write.
	daemonExited := make(chan struct{})
	var daemonErr error
	go func() {
		daemonErr = cmdDaemon.Wait()
		close(daemonExited)
	}()

	// daemonDied reports a daemon that exited during the readiness poll. A
	// daemon that dies this early (bad datadir permissions are the classic
	// cause) can never become connectable, so the poll aborts immediately
	// instead of waiting out its full timeout. A clean exit (nil error)
	// during startup is still a failure.
	daemonDied := func() error {
		status := "exit status 0"
		if daemonErr != nil {
			status = daemonErr.Error()
		}
		return fmt.Errorf("temporary database server exited during startup (%s); check the recovery log at %s", status, logFilePath)
	}

	// Defer stopping the database server cleanly. The watcher goroutine
	// performs the actual reaping; a daemon that ignores SIGTERM is killed
	// after daemonStopTimeout instead of blocking RunPITR forever.
	defer func() {
		slog.Info("Stopping temporary database server...")
		if cmdDaemon.Process != nil {
			// A failed signal usually means the process already exited and
			// was reaped by the watcher goroutine; a live-but-ignoring
			// daemon is handled by the kill fallback below.
			if err := cmdDaemon.Process.Signal(syscall.SIGTERM); err != nil {
				slog.Info("SIGTERM to database server failed; it likely already stopped", "error", err)
			}
		}
		select {
		case <-daemonExited:
		case <-time.After(daemonStopTimeout):
			slog.Warn("Database server did not stop in time, sending SIGKILL", "timeout", daemonStopTimeout)
			_ = cmdDaemon.Process.Kill()
			<-daemonExited
		}
		if daemonErr != nil {
			slog.Info("Database server stopped", "status", daemonErr.Error())
		} else {
			slog.Info("Database server stopped cleanly")
		}
	}()

	slog.Info("Checking for database connection (polling up to 2 minutes)...")
	connected := false
	var dbErr error
	deadline := time.Now().Add(2 * time.Minute)
	iteration := 0
	for time.Now().Before(deadline) {
		iteration++
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-daemonExited:
			return daemonDied()
		default:
		}
		remaining := time.Until(deadline)
		attemptTimeout := min(remaining, pitrConnectTimeout)
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		db, err := cfg.ConnectDB(attemptCtx)
		cancel()
		if err == nil {
			_ = db.Close()
			connected = true
			break
		}
		dbErr = err
		slog.Info("Waiting for MariaDB server to start", "iteration", iteration)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-daemonExited:
			return daemonDied()
		case <-time.After(5 * time.Second):
		}
	}

	if !connected {
		return fmt.Errorf("database server failed to start or connection timed out: %w. Check logs at %s", dbErr, logFilePath)
	}
	slog.Info("Connected to MariaDB server. Applying binary logs...")

	// 4. Retrieve binary logs to apply
	binlogStartFile := baseBackup.BinlogFile

	binlogsDir := filepath.Join(cfg.BackupDir, "binlogs")
	binlogsToApply, err := getBinlogFilesToApply(binlogsDir, binlogStartFile)
	if err != nil {
		return fmt.Errorf("failed to resolve binary logs to apply: %w", err)
	}

	// The archive must contain the start file itself. Without it, the events
	// between the backup's end and the next archived binlog are unrecoverable
	// and replay would silently skip them.
	startFilePresent := false
	for _, p := range binlogsToApply {
		if binlogBaseName(filepath.Base(p)) == binlogStartFile {
			startFilePresent = true
			break
		}
	}
	if !startFilePresent {
		return fmt.Errorf("archived binlogs do not contain start file %q; cannot safely replay to target time", binlogStartFile)
	}

	slog.Info("Applying binary logs", "start_file", binlogStartFile, "start_gtid", baseBackup.Gtid)
	slog.Info("Binlog files to process", "files", binlogsToApply)

	// Decompress compressed binlog files to a temporary directory for processing
	tmpBinlogDir := filepath.Join(cfg.BackupDir, "pitr_binlogs_tmp")
	if err := os.RemoveAll(tmpBinlogDir); err != nil {
		return fmt.Errorf("failed to clean temporary binlog directory: %w", err)
	}
	if err := os.MkdirAll(tmpBinlogDir, 0755); err != nil {
		return fmt.Errorf("failed to create temporary binlog directory: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(tmpBinlogDir)
	}()

	var decompressedFiles []string
	for _, compressedPath := range binlogsToApply {
		if err := ctx.Err(); err != nil {
			return err
		}
		filename := filepath.Base(compressedPath)
		baseName := binlogBaseName(filename)
		decompressedPath := filepath.Join(tmpBinlogDir, baseName)

		if err := decompressFile(ctx, compressedPath, decompressedPath); err != nil {
			return fmt.Errorf("failed to decompress binlog file %s: %w", filename, err)
		}
		decompressedFiles = append(decompressedFiles, decompressedPath)
	}

	// 5. Construct mariadb-binlog and mariadb command execution pipeline
	// Formats the stop datetime for mariadb-binlog. mariadb-binlog compares
	// --stop-datetime against event timestamps in the host's local timezone,
	// so render the target in Local to keep the replay boundary identical to
	// the instant used to select the base backup.
	// Standard format "YYYY-MM-DD HH:MM:SS"
	stopTimeStr := targetTime.Local().Format("2006-01-02 15:04:05")

	// The replay boundary is GTID-based. mariadb-binlog accepts a GTID list in
	// --start-position (events up to and including those GTIDs are skipped —
	// "the GTID binlog state the replica is already aware of", supported since
	// MariaDB 10.8), while mysqlbinlog filters with --exclude-gtids instead.
	binlogArgs := []string{
		fmt.Sprintf("--stop-datetime=%s", stopTimeStr),
	}
	if cfg.BinlogBin == "mysqlbinlog" {
		binlogArgs = append(binlogArgs, "--exclude-gtids="+baseBackup.Gtid)
	} else {
		binlogArgs = append(binlogArgs, "--start-position="+baseBackup.Gtid)
	}
	binlogArgs = append(binlogArgs, decompressedFiles...)

	mariadbArgs := cfg.GetCommonArgs()

	cmdBinlog := exec.CommandContext(ctx, cfg.BinlogBin, binlogArgs...)
	cmdMariaDB := exec.CommandContext(ctx, cfg.ClientBin, mariadbArgs...)
	if cfg.Password != "" {
		cmdMariaDB.Env = append(os.Environ(), "MYSQL_PWD="+cfg.Password)
	}

	// Setup pipeline
	pipe, err := cmdBinlog.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe for mariadb-binlog: %w", err)
	}
	cmdMariaDB.Stdin = pipe

	// Capture errors
	cmdBinlog.Stderr = os.Stderr
	cmdMariaDB.Stderr = os.Stderr
	cmdMariaDB.Stdout = os.Stdout

	slog.Info("Running binlog replay pipeline",
		"binlog_bin", cfg.BinlogBin, "binlog_args", binlogArgs,
		"client_bin", cfg.ClientBin, "client_args", mariadbArgs)

	if err := cmdBinlog.Start(); err != nil {
		return fmt.Errorf("failed to start %s: %w", cfg.BinlogBin, err)
	}

	if err := cmdMariaDB.Start(); err != nil {
		_ = cmdBinlog.Process.Kill()
		_ = cmdBinlog.Wait()
		return fmt.Errorf("failed to start %s client: %w", cfg.ClientBin, err)
	}

	if err := cmdBinlog.Wait(); err != nil {
		return fmt.Errorf("%s failed; binlog replay may be incomplete: %w", cfg.BinlogBin, err)
	}

	if err := cmdMariaDB.Wait(); err != nil {
		return fmt.Errorf("failed applying SQL statements via %s: %w", cfg.ClientBin, err)
	}

	slog.Info("PITR recovery completed successfully.")
	return nil
}

func decompressFile(ctx context.Context, src, dst string) error {
	comp := compressorForArchive(src)
	outFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = outFile.Close() }()

	decompArgs := append(slices.Clone(comp.DecompressArgs), src)
	cmdDecomp := exec.CommandContext(ctx, comp.Name, decompArgs...)
	cmdDecomp.Stdout = outFile
	cmdDecomp.Stderr = os.Stderr

	if err := cmdDecomp.Run(); err != nil {
		return err
	}
	if err := outFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync decompressed file %s: %w", dst, err)
	}
	return outFile.Close()
}

func findServerBinary() (string, error) {
	if p, err := exec.LookPath("mariadbd"); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("mysqld"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("neither mariadbd nor mysqld binary found in PATH")
}
