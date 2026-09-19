package mbkp

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ParseRetentionDuration parses a duration string, supporting 'd' for days (e.g. '7d', '30d').
func ParseRetentionDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration string")
	}

	lastChar := s[len(s)-1]
	if lastChar == 'd' || lastChar == 'D' {
		valStr := s[:len(s)-1]
		val, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid days format %q: %w", s, err)
		}
		if val < 0 {
			return 0, fmt.Errorf("duration cannot be negative: %s", s)
		}
		const maxDays = int64(math.MaxInt64 / int64(24*time.Hour))
		if val > maxDays {
			return 0, fmt.Errorf("duration %q is out of range (max %d days)", s, maxDays)
		}
		return time.Duration(val) * 24 * time.Hour, nil
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("duration cannot be negative: %s", s)
	}
	return d, nil
}

// resolveChainInMemory resolves the lineage of targetID using an in-memory backup map.
func resolveChainInMemory(backupMap map[string]BackupMetadata, targetID string) ([]BackupMetadata, error) {
	var chain []BackupMetadata
	currID := targetID
	visited := make(map[string]bool)
	for currID != "" {
		if !isValidBackupID(currID) {
			return nil, fmt.Errorf("invalid backup ID %q in lineage chain", currID)
		}
		if visited[currID] {
			return nil, fmt.Errorf("cycle detected in parent chain at %q", currID)
		}
		visited[currID] = true

		b, exists := backupMap[currID]
		if !exists || b.Status != "completed" {
			return nil, fmt.Errorf("backup ID %q in lineage chain not found or not completed", currID)
		}
		chain = append(chain, b)
		currID = b.ParentID
	}

	// Reverse: target -> root to root -> target
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}

	if len(chain) == 0 {
		return nil, fmt.Errorf("empty chain resolved")
	}
	if chain[0].Type != "full" {
		return nil, fmt.Errorf("chain must start with a full backup, got %q", chain[0].Type)
	}

	return chain, nil
}

// staleTempMaxAge bounds how old a temporary artifact must be before the
// purge sweep removes it. Purge holds the exclusive directory lock, so no
// other mbkp run can be active and every match is a leftover from a crashed
// or killed run; the age threshold is conservative extra safety for fresh
// artifacts an operator may still be using (e.g. a prepare-only directory
// just handed over by `restore --prepare-only`).
const staleTempMaxAge = 24 * time.Hour

// staleTempPatterns matches the temporary artifacts mbkp creates inside the
// backup directory while a run is in flight. In-process defers clean them up
// on ordinary failures, but a SIGKILL/OOM skips those defers — without a
// sweep the leftovers would grow without bound.
var staleTempPatterns = []string{
	"lsn_tmp_*",                        // per-run --extra-lsndir output (backup.go)
	"target_tmp_*",                     // per-run --target-dir scratch (backup.go)
	"incbase_tmp_*",                    // materialized parent checkpoints (backup.go)
	"prepare_*",                        // prepared chain and extracted incrementals (restore.go; also prepare_inc_temp_*)
	"pitr_binlogs_tmp",                 // decompressed binlogs (pitr.go)
	filepath.Join("binlogs", "*.part"), // partial binlog archives (binlog.go)
}

// sweepStaleTempArtifacts removes temporary directories and partial binlog
// archives left behind by crashed or killed mbkp runs. Removal failures are
// logged, never fatal: the purge must not abort over the sweep. Honors
// dryRun by only logging what would be removed.
func sweepStaleTempArtifacts(ctx context.Context, backupDir string, dryRun bool) {
	staleCutoff := time.Now().Add(-staleTempMaxAge)

	seen := make(map[string]bool) // prepare_* also matches prepare_inc_temp_*
	for _, pattern := range staleTempPatterns {
		matches, err := filepath.Glob(filepath.Join(backupDir, pattern))
		if err != nil {
			slog.Warn("failed to scan for stale temporary artifacts", "pattern", pattern, "error", err)
			continue
		}
		for _, path := range matches {
			if seen[path] {
				continue
			}
			seen[path] = true

			if err := ctx.Err(); err != nil {
				return
			}

			info, err := os.Stat(path)
			if err != nil {
				continue // vanished mid-sweep
			}
			if info.ModTime().After(staleCutoff) {
				continue
			}

			if dryRun {
				slog.Info("Would remove stale temporary artifact", "path", path, "mod_time", info.ModTime().Format(time.RFC3339), "dry_run", true)
				continue
			}
			slog.Info("Removing stale temporary artifact", "path", path, "mod_time", info.ModTime().Format(time.RFC3339))
			var rmErr error
			if info.IsDir() {
				rmErr = os.RemoveAll(path)
			} else {
				rmErr = os.Remove(path)
			}
			if rmErr != nil {
				slog.Error("failed to remove stale temporary artifact", "path", path, "error", rmErr)
			}
		}
	}
}

// PurgeBackups sweeps stale temporary artifacts from crashed runs, scans backups,
// cleans up missing ones, applies the retention policy, and deletes expired files/metadata.
func PurgeBackups(ctx context.Context, cfg *Config, retentionStr string, dryRun bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	retention, err := ParseRetentionDuration(retentionStr)
	if err != nil {
		return fmt.Errorf("failed to parse retention duration %q: %w", retentionStr, err)
	}

	cutoff := time.Now().Add(-retention)
	slog.Info("Starting purge", "retention", retention, "cutoff", cutoff.Format(time.RFC3339), "dry_run", dryRun)

	// 1. Sweep stale temporary artifacts left by crashed runs. This runs
	// before the catalog is even opened so reclamation does not depend on
	// backups.db being readable.
	sweepStaleTempArtifacts(ctx, cfg.BackupDir, dryRun)

	metaData, err := LoadMetadata(cfg.BackupDir)
	if err != nil {
		return fmt.Errorf("failed to load backup metadata: %w", err)
	}

	// 2. External Deletion Scan: Check if backup files are deleted outside mbkp
	var activeBackups []BackupMetadata
	for _, b := range metaData.Backups {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := validateCatalogPath(cfg.BackupDir, b.Path); err != nil {
			slog.Error("catalog row has an invalid archive path; skipping it (metadata retained)", "id", b.ID, "path", b.Path, "error", err)
			continue
		}

		archivePath := filepath.Join(cfg.BackupDir, b.Path)
		if _, err := os.Stat(archivePath); os.IsNotExist(err) {
			if dryRun {
				slog.Warn("Backup archive file not found on disk; would clean up metadata", "id", b.ID, "path", archivePath, "dry_run", true)
			} else {
				slog.Warn("Backup archive file not found on disk; cleaning up metadata", "id", b.ID, "path", archivePath, "dry_run", false)
				if err := DeleteBackup(cfg.BackupDir, b.ID); err != nil {
					slog.Error("failed to delete backup metadata", "id", b.ID, "error", err)
				}
			}
		} else {
			activeBackups = append(activeBackups, b)
		}
	}

	// Build in-memory map of active backups
	backupMap := make(map[string]BackupMetadata)
	for _, b := range activeBackups {
		backupMap[b.ID] = b
	}

	// 3. Retention policy evaluation
	keepIDs := make(map[string]bool)

	for _, b := range activeBackups {
		if err := ctx.Err(); err != nil {
			return err
		}

		if b.Status == "completed" {
			// Check if the backup itself is within the retention window
			if !b.EndTime.Before(cutoff) {
				// To keep this backup, we must be able to restore it (entire chain intact)
				chain, err := resolveChainInMemory(backupMap, b.ID)
				if err != nil {
					slog.Warn("Backup is within retention window but cannot be restored; eligible for purging", "id", b.ID, "error", err)
				} else {
					for _, bInChain := range chain {
						keepIDs[bInChain.ID] = true
					}
				}
			}
		} else {
			// For failed or in_progress backups, keep them if they started within the retention window
			if !b.StartTime.Before(cutoff) {
				keepIDs[b.ID] = true
			}
		}
	}

	// 4. Purge backups that should not be kept
	for _, b := range metaData.Backups {
		if err := ctx.Err(); err != nil {
			return err
		}

		if keepIDs[b.ID] {
			continue
		}

		// This loop walks every catalog row (not just the active ones), so it
		// is the last line of defense: a row with an escaping Path is refused
		// entirely — neither its file nor its metadata is touched.
		if err := validateCatalogPath(cfg.BackupDir, b.Path); err != nil {
			slog.Error("catalog row has an invalid archive path; skipping it (metadata retained)", "id", b.ID, "path", b.Path, "error", err)
			continue
		}

		archivePath := filepath.Join(cfg.BackupDir, b.Path)
		if dryRun {
			slog.Info("Would purge backup", "id", b.ID, "type", b.Type, "end_time", b.EndTime.Format(time.RFC3339), "dry_run", true)
			if _, err := os.Stat(archivePath); err == nil {
				slog.Info("Would delete physical archive file", "path", archivePath, "dry_run", true)
			}
			slog.Info("Would delete metadata record", "id", b.ID, "dry_run", true)
		} else {
			slog.Info("Purging backup", "id", b.ID, "type", b.Type, "end_time", b.EndTime.Format(time.RFC3339), "dry_run", false)
			if _, err := os.Stat(archivePath); err == nil {
				slog.Info("Deleting physical archive file", "path", archivePath, "dry_run", false)
				if err := os.Remove(archivePath); err != nil {
					slog.Error("failed to delete archive file", "path", archivePath, "error", err)
				}
			}
			slog.Info("Deleting metadata record", "id", b.ID, "dry_run", false)
			if err := DeleteBackup(cfg.BackupDir, b.ID); err != nil {
				slog.Error("failed to delete metadata record", "id", b.ID, "error", err)
			}
		}
	}

	// 5. Purge archived binlog files
	// Find oldest kept completed backup
	var oldestKeptBackup *BackupMetadata
	for _, b := range activeBackups {
		if keepIDs[b.ID] && b.Status == "completed" {
			if oldestKeptBackup == nil || b.StartTime.Before(oldestKeptBackup.StartTime) {
				bCopy := b
				oldestKeptBackup = &bCopy
			}
		}
	}

	if oldestKeptBackup != nil && oldestKeptBackup.BinlogFile == "" {
		slog.Warn("Oldest kept backup has no recorded binlog file; retaining all archived binlogs to prevent data loss", "backup_id", oldestKeptBackup.ID)
	}

	binlogsDir := filepath.Join(cfg.BackupDir, "binlogs")
	entries, err := os.ReadDir(binlogsDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No binlogs directory, nothing to do
			return nil
		}
		return fmt.Errorf("failed to read binlogs directory: %w", err)
	}

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}

		if entry.IsDir() || !isBinlogFile(entry.Name()) {
			continue
		}

		name := entry.Name()
		binlogPath := filepath.Join(binlogsDir, name)

		info, err := entry.Info()
		if err != nil {
			slog.Warn("failed to get file info for binlog", "binlog", name, "error", err)
			continue
		}

		isExpired := info.ModTime().Before(cutoff)
		shouldDelete := false

		binlogNameWithoutExt := name
		if before, ok := strings.CutSuffix(binlogNameWithoutExt, ".lz4"); ok {
			binlogNameWithoutExt = before
		} else if before, ok := strings.CutSuffix(binlogNameWithoutExt, ".gz"); ok {
			binlogNameWithoutExt = before
		}

		if oldestKeptBackup == nil {
			shouldDelete = isExpired
		} else if oldestKeptBackup.BinlogFile == "" {
			shouldDelete = false
		} else {
			shouldDelete = isExpired && (binlogNameWithoutExt < oldestKeptBackup.BinlogFile)
		}

		if shouldDelete {
			if dryRun {
				slog.Info("Would delete archived binlog file", "path", binlogPath, "mod_time", info.ModTime().Format(time.RFC3339), "dry_run", true)
			} else {
				slog.Info("Deleting archived binlog file", "path", binlogPath, "mod_time", info.ModTime().Format(time.RFC3339), "dry_run", false)
				if err := os.Remove(binlogPath); err != nil {
					slog.Error("failed to delete binlog file", "path", binlogPath, "error", err)
				}
			}
		}
	}

	return nil
}
