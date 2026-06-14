package mbkp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

// ListBackups prints all backups in the requested output format ("table" or "json").
func ListBackups(cfg *Config, format string) error {
	meta, err := LoadMetadata(cfg.BackupDir)
	if err != nil {
		return fmt.Errorf("failed to load backup metadata: %w", err)
	}

	switch strings.ToLower(format) {
	case "json":
		return listJSON(meta)
	default:
		return listTable(cfg.BackupDir, meta)
	}
}

// listJSON marshals the full backup list as pretty-printed JSON to stdout.
func listJSON(meta *Metadata) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(meta.Backups)
}

// listTable renders the backup list as a human-readable aligned table.
func listTable(backupDir string, meta *Metadata) error {
	if len(meta.Backups) == 0 {
		fmt.Println("No backups found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = w.Flush() }()

	// Header
	_, _ = fmt.Fprintln(w, "ID\tTYPE\tSTATUS\tSTARTED\tDURATION\tSIZE\tBINLOG\tPARENT")
	_, _ = fmt.Fprintln(w, strings.Repeat("-", 20)+"\t"+
		strings.Repeat("-", 11)+"\t"+
		strings.Repeat("-", 11)+"\t"+
		strings.Repeat("-", 19)+"\t"+
		strings.Repeat("-", 9)+"\t"+
		strings.Repeat("-", 9)+"\t"+
		strings.Repeat("-", 24)+"\t"+
		strings.Repeat("-", 20))

	for _, b := range meta.Backups {
		started := b.StartTime.Local().Format("2006-01-02 15:04:05")

		duration := "-"
		if b.Status == "completed" || b.Status == "failed" {
			d := b.EndTime.Sub(b.StartTime).Round(time.Second)
			duration = d.String()
		}

		size := archiveSize(backupDir, b.Path)

		binlog := "-"
		if b.BinlogFile != "" {
			binlog = fmt.Sprintf("%s:%d", b.BinlogFile, b.BinlogPos)
		}

		parent := "-"
		if b.ParentID != "" {
			parent = b.ParentID
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			b.ID, b.Type, b.Status, started, duration, size, binlog, parent)
	}

	return nil
}

// archiveSize returns a human-readable size string for the archive file stored in path.
// Returns "-" if the file cannot be stat'd (e.g. in_progress or failed backups).
func archiveSize(backupDir, relPath string) string {
	if relPath == "" {
		return "-"
	}
	info, err := os.Stat(filepath.Join(backupDir, relPath))
	if err != nil {
		return "-"
	}
	return humanBytes(info.Size())
}

// humanBytes formats a byte count as a human-readable string (KiB, MiB, GiB).
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
