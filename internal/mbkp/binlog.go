package mbkp

import (
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
)

// BackupBinlogs connects to the database, flushes logs, and copies all closed binary logs to the archive directory
func BackupBinlogs(cfg *Config) error {
	db, err := cfg.ConnectDB()
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer func() { _ = db.Close() }()

	// 1. Check if binlog is enabled
	var varName, logBinVal string
	err = db.QueryRow("SHOW VARIABLES LIKE 'log_bin'").Scan(&varName, &logBinVal)
	if err != nil {
		return fmt.Errorf("failed to query log_bin status: %w", err)
	}
	if logBinVal != "ON" {
		return fmt.Errorf("binary logging (log_bin) is not enabled on this MariaDB server")
	}

	// 2. Flush binary logs to close the current active one and open a new one
	slog.Info("Flushing binary logs on MariaDB server...")
	_, err = db.Exec("FLUSH BINARY LOGS")
	if err != nil {
		return fmt.Errorf("failed to execute FLUSH BINARY LOGS: %w", err)
	}

	// 3. Get log_bin_basename to locate the binlog files on disk
	var logBinBasename string
	err = db.QueryRow("SHOW VARIABLES LIKE 'log_bin_basename'").Scan(&varName, &logBinBasename)
	if err != nil {
		return fmt.Errorf("failed to query log_bin_basename: %w", err)
	}
	sourceDir := filepath.Dir(logBinBasename)

	// 4. Retrieve list of all binary logs
	rows, err := db.Query("SHOW BINARY LOGS")
	if err != nil {
		return fmt.Errorf("failed to retrieve list of binary logs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type binlogFileInfo struct {
		LogName string
		Size    int64
	}
	var binlogs []binlogFileInfo
	for rows.Next() {
		var name string
		var size int64
		// In some MariaDB versions, SHOW BINARY LOGS has columns: Log_name, File_size, Encrypted
		// We scan the first two columns which are always Log_name and File_size
		var encrypted sql.RawBytes // optional column
		columns, err := rows.Columns()
		if err != nil {
			return err
		}
		if len(columns) >= 3 {
			err = rows.Scan(&name, &size, &encrypted)
		} else {
			err = rows.Scan(&name, &size)
		}
		if err != nil {
			return fmt.Errorf("failed to scan binary log row: %w", err)
		}
		binlogs = append(binlogs, binlogFileInfo{LogName: name, Size: size})
	}

	if len(binlogs) > 0 {
		// Don't backup the latest binary log created after flush
		binlogs = binlogs[:len(binlogs)-1]
	}

	// 5. Create backup binlogs directory
	binlogsBackupDir := filepath.Join(cfg.BackupDir, "binlogs")
	if err := os.MkdirAll(binlogsBackupDir, 0755); err != nil {
		return fmt.Errorf("failed to create binlogs backup directory: %w", err)
	}

	comp := detectCompressor()
	var ext string
	if comp.Name == "lz4" {
		ext = ".lz4"
	} else {
		ext = ".gz"
	}

	slog.Info("Archiving binlog files", "count", len(binlogs), "source_dir", sourceDir, "dest_dir", binlogsBackupDir)

	for _, binlog := range binlogs {
		srcPath := filepath.Join(sourceDir, binlog.LogName)
		dstPath := filepath.Join(binlogsBackupDir, binlog.LogName+ext)

		// Check if it already exists
		if _, err := os.Stat(dstPath); err == nil {
			slog.Info("Binlog already archived, skipping", "binlog", binlog.LogName)
			continue
		}

		slog.Info("Copying and compressing binlog file", "binlog", binlog.LogName)
		if err := compressAndCopyFile(srcPath, dstPath, comp); err != nil {
			return fmt.Errorf("failed to archive binlog file %s: %w", binlog.LogName, err)
		}
	}

	slog.Info("Binlog archiving completed successfully.")
	return nil
}

func compressAndCopyFile(src, dst string, comp Compressor) error {
	inFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = inFile.Close() }()

	outFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = outFile.Close() }()

	cmdCompress := exec.Command(comp.Name, comp.CompressArgs...)
	cmdCompress.Stdin = inFile
	cmdCompress.Stdout = outFile
	cmdCompress.Stderr = os.Stderr

	return cmdCompress.Run()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
