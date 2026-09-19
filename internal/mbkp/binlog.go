package mbkp

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
)

// BackupBinlogs connects to the database, flushes logs, and copies all closed binary logs to the archive directory
func BackupBinlogs(ctx context.Context, cfg *Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	db, err := cfg.ConnectDB(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer func() { _ = db.Close() }()

	// 1. Check if binlog is enabled
	var varName, logBinVal string
	err = db.QueryRowContext(ctx, "SHOW VARIABLES LIKE 'log_bin'").Scan(&varName, &logBinVal)
	if err != nil {
		return fmt.Errorf("failed to query log_bin status: %w", err)
	}
	if logBinVal != "ON" {
		return fmt.Errorf("binary logging (log_bin) is not enabled on this MariaDB server")
	}

	// 2. Flush binary logs to close the current active one and open a new one
	slog.Info("Flushing binary logs on MariaDB server...")
	_, err = db.ExecContext(ctx, "FLUSH BINARY LOGS")
	if err != nil {
		return fmt.Errorf("failed to execute FLUSH BINARY LOGS: %w", err)
	}

	// 3. Get log_bin_basename to locate the binlog files on disk
	var logBinBasename string
	err = db.QueryRowContext(ctx, "SHOW VARIABLES LIKE 'log_bin_basename'").Scan(&varName, &logBinBasename)
	if err != nil {
		return fmt.Errorf("failed to query log_bin_basename: %w", err)
	}
	sourceDir := filepath.Dir(logBinBasename)

	// 4. Retrieve list of all binary logs
	rows, err := db.QueryContext(ctx, "SHOW BINARY LOGS")
	if err != nil {
		return fmt.Errorf("failed to retrieve list of binary logs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	binlogs, err := scanBinaryLogs(rows)
	if err != nil {
		return err
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

	comp := compressorDetector()
	var ext string
	if comp.Name == "lz4" {
		ext = ".lz4"
	} else {
		ext = ".gz"
	}

	slog.Info("Archiving binlog files", "count", len(binlogs), "source_dir", sourceDir, "dest_dir", binlogsBackupDir)

	for _, binlog := range binlogs {
		if err := ctx.Err(); err != nil {
			return err
		}

		srcPath := filepath.Join(sourceDir, binlog.LogName)
		dstPath := filepath.Join(binlogsBackupDir, binlog.LogName+ext)

		// Skip if the log is already archived under any compression variant:
		// earlier runs may have used a different compressor (e.g. gzip before
		// lz4 was installed). Archiving a second variant would leave two
		// archives of the same binlog, which PITR would decompress into the
		// same output path and hand to the binlog tool twice.
		alreadyArchived := false
		for _, variant := range []string{binlog.LogName + ".lz4", binlog.LogName + ".gz"} {
			if _, err := os.Stat(filepath.Join(binlogsBackupDir, variant)); err == nil {
				alreadyArchived = true
				break
			}
		}
		if alreadyArchived {
			slog.Info("Binlog already archived, skipping", "binlog", binlog.LogName)
			continue
		}

		slog.Info("Copying and compressing binlog file", "binlog", binlog.LogName)
		// Compress to a .part file and rename it into place so a failed or
		// killed run never leaves a truncated archive that later runs would
		// skip as "already archived".
		tmpPath := dstPath + ".part"
		if err := compressAndCopyFile(ctx, srcPath, tmpPath, comp); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("failed to archive binlog file %s: %w", binlog.LogName, err)
		}
		if err := os.Rename(tmpPath, dstPath); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("failed to finalize archived binlog file %s: %w", binlog.LogName, err)
		}
	}

	slog.Info("Binlog archiving completed successfully.")
	return nil
}

// binlogFileInfo carries the binlog name from a SHOW BINARY LOGS row. The
// File_size column is still scanned (the row shape requires it) but its value
// is not retained — nothing downstream consumes it.
type binlogFileInfo struct {
	LogName string
}

// scanBinaryLogs reads rows from SHOW BINARY LOGS, handles both 2-column
// (Log_name, File_size) and 3-column (Log_name, File_size, Encrypted) formats,
// and checks rows.Err() so iteration failures (e.g. dropped network connection)
// fail fast rather than silently proceeding with a truncated list of binlogs.
func scanBinaryLogs(rows *sql.Rows) ([]binlogFileInfo, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("failed to get binary log columns: %w", err)
	}

	var binlogs []binlogFileInfo
	for rows.Next() {
		var name string
		// File_size is scanned to consume the column but discarded — see the
		// binlogFileInfo comment.
		var size int64
		// In some MariaDB versions, SHOW BINARY LOGS has columns: Log_name, File_size, Encrypted
		// We scan the first two columns which are always Log_name and File_size
		var encrypted sql.RawBytes // optional column
		if len(columns) >= 3 {
			err = rows.Scan(&name, &size, &encrypted)
		} else {
			err = rows.Scan(&name, &size)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to scan binary log row: %w", err)
		}
		binlogs = append(binlogs, binlogFileInfo{LogName: name})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating binary log rows: %w", err)
	}
	return binlogs, nil
}

func compressAndCopyFile(ctx context.Context, src, dst string, comp Compressor) error {
	inFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = inFile.Close() }()

	outFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = outFile.Close() }()

	cmdCompress := exec.CommandContext(ctx, comp.Name, comp.CompressArgs...)
	cmdCompress.Stdin = inFile
	cmdCompress.Stdout = outFile
	cmdCompress.Stderr = os.Stderr

	return cmdCompress.Run()
}
