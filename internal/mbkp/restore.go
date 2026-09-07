package mbkp

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// copyDir recursively copies a directory tree, preserving permissions.
// Still used for non-archive scenarios (e.g., future extensions).
func copyDir(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, info.Mode()); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if err := copyFileWithMode(srcPath, dstPath, info.Mode()); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyFileWithMode(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// PrepareChain resolves the backup lineage for backupID, extracts and prepares the backup chain,
// and returns the path to the prepared directory ready for copy-back.
func PrepareChain(cfg *Config, backupID string) (string, error) {
	// 1. Resolve the chain of backups
	chain, err := ResolveChain(cfg.BackupDir, backupID)
	if err != nil {
		return "", fmt.Errorf("failed to resolve backup chain for ID %q: %w", backupID, targetIDError(backupID, err))
	}

	slog.Info("Resolved backup chain", "length", len(chain))
	for i, b := range chain {
		slog.Info("Backup in chain", "index", i, "type", b.Type, "id", b.ID, "path", b.Path)
	}

	// 2. Create the prepare directory and extract the base full backup into it
	prepareDir := filepath.Join(cfg.BackupDir, "prepare_"+backupID)
	if err := os.RemoveAll(prepareDir); err != nil {
		return "", fmt.Errorf("failed to clean existing prepare directory: %w", err)
	}

	fullBackup := chain[0]
	fullArchive := filepath.Join(cfg.BackupDir, fullBackup.Path)
	slog.Info("Extracting base full backup to prepare directory", "id", fullBackup.ID, "prepare_dir", prepareDir)
	if err := extractArchive(cfg.StreamBin, fullArchive, prepareDir); err != nil {
		return "", fmt.Errorf("failed to extract full backup archive: %w", err)
	}

	// Track temporary incremental directories so they are cleaned up on exit/error
	var tempDirs []string
	defer func() {
		for _, dir := range tempDirs {
			slog.Info("Cleaning up temporary incremental directory", "path", dir)
			if err := os.RemoveAll(dir); err != nil {
				slog.Warn("failed to clean up temporary directory", "path", dir, "error", err)
			}
		}
	}()

	// 3. Run prepare commands
	if len(chain) == 1 {
		slog.Info("Preparing base full backup...")
		args := []string{"--prepare", "--target-dir=" + prepareDir}
		cmd := exec.Command(cfg.BackupBin, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		slog.Info("Running command", "command", cfg.BackupBin, "args", args)
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("failed to prepare base full backup: %w", err)
		}
	} else {
		slog.Info("Preparing base full backup (for incrementals)...")
		args := []string{"--prepare"}
		if cfg.BackupBin == "xtrabackup" {
			args = append(args, "--apply-log-only")
		}
		args = append(args, "--target-dir="+prepareDir)

		cmd := exec.Command(cfg.BackupBin, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		slog.Info("Running command", "command", cfg.BackupBin, "args", args)
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("failed to prepare base full backup: %w", err)
		}

		// Apply incremental backups
		for i := 1; i < len(chain); i++ {
			inc := chain[i]
			incArchive := filepath.Join(cfg.BackupDir, inc.Path)

			// Extract the incremental archive to a temporary directory.
			// mariabackup --prepare modifies/consumes files inside --incremental-dir
			// (e.g. it moves .new tablespace files). By extracting into a fresh temp dir
			// each time we keep the original archive intact for repeated restores/PITR.
			tempIncDir := filepath.Join(cfg.BackupDir, "prepare_inc_temp_"+inc.ID)
			slog.Info("Extracting incremental backup to temp dir", "id", inc.ID, "temp_dir", tempIncDir)
			if err := os.RemoveAll(tempIncDir); err != nil {
				return "", fmt.Errorf("failed to clean temporary incremental directory: %w", err)
			}
			if err := extractArchive(cfg.StreamBin, incArchive, tempIncDir); err != nil {
				return "", fmt.Errorf("failed to extract incremental archive %s: %w", inc.ID, err)
			}
			tempDirs = append(tempDirs, tempIncDir)

			slog.Info("Applying incremental backup", "index", i, "total", len(chain)-1, "id", inc.ID)
			args := []string{"--prepare"}
			if cfg.BackupBin == "xtrabackup" && i < len(chain)-1 {
				args = append(args, "--apply-log-only")
			}
			args = append(args, "--target-dir="+prepareDir, "--incremental-dir="+tempIncDir)

			cmd := exec.Command(cfg.BackupBin, args...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			slog.Info("Running command", "command", cfg.BackupBin, "args", args)
			if err := cmd.Run(); err != nil {
				return "", fmt.Errorf("failed to apply incremental backup %s: %w", inc.ID, err)
			}
		}

		slog.Info("Finalizing prepare (rolling back uncommitted transactions)...")
		argsFinal := []string{"--prepare", "--target-dir=" + prepareDir}
		cmdFinal := exec.Command(cfg.BackupBin, argsFinal...)
		cmdFinal.Stdout = os.Stdout
		cmdFinal.Stderr = os.Stderr
		slog.Info("Running command", "command", cfg.BackupBin, "args", argsFinal)
		if err := cmdFinal.Run(); err != nil {
			return "", fmt.Errorf("failed to finalize prepared backups: %w", err)
		}
	}

	return prepareDir, nil
}

func targetIDError(backupID string, err error) error {
	if backupID == "" {
		return fmt.Errorf("no backup ID specified and search failed: %w", err)
	}
	return err
}

const backupMyCnfFile = "backup-my.cnf"
const autoCnfFile = "auto.cnf"

// isValidServerUUID reports whether s is a canonical 8-4-4-4-12 hexadecimal
// UUID, the shape MySQL writes for server-uuid.
func isValidServerUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// readServerUUID extracts the backed-up server's UUID from the backup-my.cnf
// file xtrabackup writes into every backup (auto.cnf itself is deliberately
// excluded from xtrabackup archives). Returns an empty string when the file or
// the server-uuid line is missing or malformed; callers treat that as "no
// recorded identity" and let the restored server generate a fresh UUID.
func readServerUUID(prepareDir string) string {
	path := filepath.Join(prepareDir, backupMyCnfFile)
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("failed to read backup-my.cnf; the restored server will generate a fresh UUID", "path", path, "error", err)
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		name, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		// xtrabackup writes the key as "server_uuid" (underscore) while
		// auto.cnf uses "server-uuid" (hyphen); accept both spellings.
		name = strings.ReplaceAll(strings.TrimSpace(name), "_", "-")
		if !strings.EqualFold(name, "server-uuid") {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if !isValidServerUUID(value) {
			slog.Warn("backup-my.cnf contains a malformed server-uuid; the restored server will generate a fresh UUID", "value", value)
			return ""
		}
		return value
	}
	slog.Warn("backup-my.cnf has no server-uuid entry; the restored server will generate a fresh UUID", "path", path)
	return ""
}

// writeAutoCnf writes <datadir>/auto.cnf with the backed-up server-uuid so the
// restored server reuses the original's GTID identity. Without it every
// restore boots with a fresh UUID, fragmenting gtid_executed across one new
// UUID per recovery generation.
func writeAutoCnf(datadir, uuid string) error {
	content := "[auto]\nserver-uuid=" + uuid + "\n"
	return os.WriteFile(filepath.Join(datadir, autoCnfFile), []byte(content), 0644)
}

// restoreServerUUID re-establishes the backed-up server's identity after
// copy-back for MySQL/Percona backups (MariaDB GTIDs do not use UUIDs, so the
// MariaDB path is left untouched). With newServerUUID set, any auto.cnf that
// survived copy-back is removed so the server generates a fresh UUID instead.
func restoreServerUUID(cfg *Config, prepareDir, datadir string, newServerUUID bool) error {
	if cfg.BackupBin != "xtrabackup" {
		return nil
	}
	autoCnfPath := filepath.Join(datadir, autoCnfFile)

	if newServerUUID {
		// The operator explicitly asked for a fresh identity (e.g. the
		// restored server will run alongside the original as a clone).
		if _, err := os.Stat(autoCnfPath); err == nil {
			if err := os.Remove(autoCnfPath); err != nil {
				return fmt.Errorf("failed to remove %s: %w", autoCnfPath, err)
			}
		}
		slog.Info("Skipping server-uuid restoration; the server will generate a fresh UUID on first start")
		return nil
	}

	uuid := readServerUUID(prepareDir)
	if uuid == "" {
		return nil
	}
	if err := writeAutoCnf(datadir, uuid); err != nil {
		return fmt.Errorf("failed to restore server-uuid via auto.cnf: %w", err)
	}
	slog.Info("Restored server-uuid from backup", "uuid", uuid, "path", autoCnfPath)
	return nil
}

func RestoreBackup(cfg *Config, backupID string, datadir string, prepareOnly bool, newServerUUID bool) error {
	// If backupID is empty, find the latest completed backup
	if backupID == "" {
		latest, err := GetLatestBackup(cfg.BackupDir)
		if err != nil {
			return fmt.Errorf("failed to look up latest backup: %w", err)
		}
		if latest == nil {
			return fmt.Errorf("no completed backups found to restore")
		}
		backupID = latest.ID
	}

	// 1. Prepare the database files
	prepareDir, err := PrepareChain(cfg, backupID)
	if err != nil {
		return err
	}

	if prepareOnly {
		slog.Info("Prepare only specified. Prepared backup files are located", "path", prepareDir)
		return nil
	}

	// Defer cleanup of the prepare directory
	defer func() {
		slog.Info("Cleaning up prepare directory", "path", prepareDir)
		_ = os.RemoveAll(prepareDir)
	}()

	// 2. Perform copy-back
	if datadir == "" {
		return fmt.Errorf("datadir must be specified for copy-back operation")
	}

	slog.Info("Restoring prepared files to data directory", "prepare_dir", prepareDir, "datadir", datadir)

	// Check if datadir exists. If it does, verify it is empty (except maybe '.' or '..')
	if info, err := os.Stat(datadir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("target datadir %s is not a directory", datadir)
		}
		entries, err := os.ReadDir(datadir)
		if err != nil {
			return fmt.Errorf("failed to read datadir: %w", err)
		}
		if len(entries) > 0 {
			return fmt.Errorf("target datadir %s is not empty. Please clear or remove it before restore", datadir)
		}
	} else if os.IsNotExist(err) {
		// Create target datadir if it doesn't exist
		if err := os.MkdirAll(datadir, 0755); err != nil {
			return fmt.Errorf("failed to create target datadir: %w", err)
		}
	}

	args := []string{"--copy-back", "--target-dir=" + prepareDir, "--datadir=" + datadir}
	cmd := exec.Command(cfg.BackupBin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	slog.Info("Running command", "command", cfg.BackupBin, "args", args)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s --copy-back failed: %w", cfg.BackupBin, err)
	}

	// Re-establish the backed-up server's UUID before any server is started on
	// the datadir (the PITR recovery daemon reads it right after this returns).
	if err := restoreServerUUID(cfg, prepareDir, datadir, newServerUUID); err != nil {
		return err
	}

	slog.Info("Restore (copy-back) completed successfully. Please check file permissions and start the MariaDB server.")
	return nil
}
