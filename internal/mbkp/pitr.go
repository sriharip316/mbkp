package mbkp

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// isBinlogFile checks if the file is a MariaDB binary log file (has a numeric extension, optionally compressed)
func isBinlogFile(filename string) bool {
	// Strip compression suffix if present
	if strings.HasSuffix(filename, ".lz4") {
		filename = strings.TrimSuffix(filename, ".lz4")
	} else if strings.HasSuffix(filename, ".gz") {
		filename = strings.TrimSuffix(filename, ".gz")
	}
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
	for _, name := range filenames {
		baseName := name
		if strings.HasSuffix(baseName, ".lz4") {
			baseName = strings.TrimSuffix(baseName, ".lz4")
		} else if strings.HasSuffix(baseName, ".gz") {
			baseName = strings.TrimSuffix(baseName, ".gz")
		}
		if baseName >= startFile {
			filtered = append(filtered, filepath.Join(binlogsDir, name))
		}
	}

	return filtered, nil
}

func RunPITR(cfg *Config, targetTime time.Time, datadir string) error {
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

	// 2. Restore the selected backup
	slog.Info("Restoring backup", "id", baseBackup.ID)
	// We restore it directly to the datadir (prepare + copy-back)
	if err := RestoreBackup(cfg, baseBackup.ID, datadir, false); err != nil {
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
		chownCmd := exec.Command("chown", "-R", "mysql:mysql", datadir)
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
			_ = exec.Command("chown", "mysql:mysql", socketDir).Run()
		}
		daemonArgs = append(daemonArgs, "--socket="+cfg.Socket)
		daemonArgs = append(daemonArgs, "--skip-networking")
	} else {
		daemonArgs = append(daemonArgs, "--port="+strconv.Itoa(cfg.Port))
		daemonArgs = append(daemonArgs, "--bind-address=127.0.0.1")
	}

	_ = os.MkdirAll(cfg.BackupDir, 0755)
	logFilePath := filepath.Join(cfg.BackupDir, "pitr_mariadbd.log")
	logFile, err := os.Create(logFilePath)
	if err != nil {
		return fmt.Errorf("failed to create recovery database log file: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	cmdDaemon := exec.Command(binary, daemonArgs...)
	cmdDaemon.Stdout = logFile
	cmdDaemon.Stderr = logFile

	slog.Info("Starting temporary database server for recovery", "binary", binary, "args", daemonArgs, "log", logFilePath)
	if err := cmdDaemon.Start(); err != nil {
		return fmt.Errorf("failed to start temporary database server: %w", err)
	}

	// Defer stopping the database server cleanly
	defer func() {
		slog.Info("Stopping temporary database server...")
		if cmdDaemon.Process != nil {
			if err := cmdDaemon.Process.Signal(syscall.SIGTERM); err != nil {
				slog.Warn("Failed to send SIGTERM to database server, attempting SIGKILL", "error", err)
				_ = cmdDaemon.Process.Kill()
			}
			if err := cmdDaemon.Wait(); err != nil {
				slog.Info("Database server stopped", "status", err.Error())
			} else {
				slog.Info("Database server stopped cleanly")
			}
		}
	}()

	slog.Info("Checking for database connection (polling up to 2 minutes)...")
	connected := false
	var dbErr error
	for i := 0; i < 24; i++ { // 24 * 5s = 120s
		db, err := cfg.ConnectDB()
		if err == nil {
			_ = db.Close()
			connected = true
			break
		}
		dbErr = err
		slog.Info("Waiting for MariaDB server to start", "iteration", i+1, "max_iterations", 24)
		time.Sleep(5 * time.Second)
	}

	if !connected {
		return fmt.Errorf("database server failed to start or connection timed out: %w. Check logs at %s", dbErr, logFilePath)
	}
	slog.Info("Connected to MariaDB server. Applying binary logs...")

	// 4. Retrieve binary logs to apply
	binlogStartFile := baseBackup.BinlogFile
	binlogStartPos := baseBackup.BinlogPos

	if binlogStartFile == "" {
		return fmt.Errorf("base backup does not contain binlog coordinates (binlog info file missing from archive)")
	}

	binlogsDir := filepath.Join(cfg.BackupDir, "binlogs")
	binlogsToApply, err := getBinlogFilesToApply(binlogsDir, binlogStartFile)
	if err != nil {
		return fmt.Errorf("failed to resolve binary logs to apply: %w", err)
	}

	if len(binlogsToApply) == 0 {
		slog.Info("No binary logs to apply. Start file not found in archived binlogs", "start_file", binlogStartFile)
		return nil
	}

	slog.Info("Applying binary logs", "start_file", binlogStartFile, "start_pos", binlogStartPos)
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
		filename := filepath.Base(compressedPath)
		baseName := filename
		if strings.HasSuffix(baseName, ".lz4") {
			baseName = strings.TrimSuffix(baseName, ".lz4")
		} else if strings.HasSuffix(baseName, ".gz") {
			baseName = strings.TrimSuffix(baseName, ".gz")
		}
		decompressedPath := filepath.Join(tmpBinlogDir, baseName)

		if err := decompressFile(compressedPath, decompressedPath); err != nil {
			return fmt.Errorf("failed to decompress binlog file %s: %w", filename, err)
		}
		decompressedFiles = append(decompressedFiles, decompressedPath)
	}

	// 5. Construct mariadb-binlog and mariadb command execution pipeline
	// Formats the stop datetime for mariadb-binlog. Note: mariadb-binlog expects local or UTC time string
	// standard format "YYYY-MM-DD HH:MM:SS"
	stopTimeStr := targetTime.Format("2006-01-02 15:04:05")

	binlogArgs := []string{
		fmt.Sprintf("--start-position=%d", binlogStartPos),
		fmt.Sprintf("--stop-datetime=%s", stopTimeStr),
	}
	binlogArgs = append(binlogArgs, decompressedFiles...)

	mariadbArgs := cfg.GetCommonArgs()

	cmdBinlog := exec.Command(cfg.BinlogBin, binlogArgs...)
	cmdMariaDB := exec.Command(cfg.ClientBin, mariadbArgs...)
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
		return fmt.Errorf("failed to start %s client: %w", cfg.ClientBin, err)
	}

	if err := cmdBinlog.Wait(); err != nil {
		slog.Warn("binlog replay tool exited with error", "binary", cfg.BinlogBin, "error", err)
	}

	if err := cmdMariaDB.Wait(); err != nil {
		return fmt.Errorf("failed applying SQL statements via %s: %w", cfg.ClientBin, err)
	}

	slog.Info("PITR recovery completed successfully.")
	return nil
}

func decompressFile(src, dst string) error {
	comp := compressorForArchive(src)
	outFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = outFile.Close() }()

	decompArgs := append(comp.DecompressArgs, src)
	cmdDecomp := exec.Command(comp.Name, decompArgs...)
	cmdDecomp.Stdout = outFile
	cmdDecomp.Stderr = os.Stderr

	return cmdDecomp.Run()
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
