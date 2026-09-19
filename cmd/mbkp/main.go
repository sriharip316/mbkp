package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/srihari/mbkp/internal/mbkp"
)

var (
	backupDir     string
	parentId      string
	outputFmt     string
	backupId      string
	datadir       string
	prepareOnly   bool
	newServerUUID bool
	targetTime    string
	retention     string
	dryRun        bool
)

// version is injected at build time using:
//
//	go build -ldflags="-X main.version=v1.2.3"
var version string = "dev"

var rootCmd = &cobra.Command{
	Use:     "mbkp",
	Version: version,
	Short:   "MariaDB Backup & Recovery Tool (mbkp)",
	Long: `MariaDB Backup & Recovery Tool (mbkp) manages MariaDB physical backups, restores, and PITR.

Environment Variables:
  Connection & Credentials (checked in order of fallback):
    Host:       MARIADB_HOST, MYSQL_HOST (default: localhost)
    Port:       MARIADB_PORT, MYSQL_PORT (default: 3306)
    User:       MARIADB_USER, MYSQL_USER (default: root)
    Password:   MARIADB_PASSWORD, MYSQL_PASSWORD, MYSQL_PWD, MARIADB_ROOT_PASSWORD, MYSQL_ROOT_PASSWORD
    Socket:     MARIADB_SOCKET, MYSQL_UNIX_PORT

  TLS / SSL Settings:
    CA Cert:    MARIADB_SSL_CA
    Cert File:  MARIADB_SSL_CERT
    Key File:   MARIADB_SSL_KEY
    Verify SSL: MARIADB_SSL_VERIFY (default: true)

  Backup Location:
    Backup Dir: MBKP_BACKUP_DIR (overridden by --backup-dir flag)`,
	SilenceErrors: true,
	SilenceUsage:  true,
	RunE: func(cmd *cobra.Command, args []string) error {
		_ = cmd.Help()
		return fmt.Errorf("subcommand required")
	},
}

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Perform a backup (full, incremental, or binlog)",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("Error: backup subcommand (full, incremental, binlog) is required.")
		fmt.Println("Usage: mbkp backup <full | incremental | binlog>")
		return fmt.Errorf("backup subcommand required")
	},
}

var backupFullCmd = &cobra.Command{
	Use:   "full",
	Short: "Perform a full physical backup of MariaDB",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := mbkp.LoadConfig(backupDir)
		if err != nil {
			slog.Error("Configuration error", "error", err)
			return err
		}
		release, err := mbkp.AcquireLock(cfg.BackupDir)
		if err != nil {
			slog.Error("Cannot start full backup", "error", err)
			return err
		}
		defer func() { _ = release() }()
		if err := mbkp.RunFullBackup(cmd.Context(), cfg); err != nil {
			slog.Error("Full backup failed", "error", err)
			return err
		}
		return nil
	},
}

var backupIncrementalCmd = &cobra.Command{
	Use:   "incremental",
	Short: "Perform an incremental physical backup",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := mbkp.LoadConfig(backupDir)
		if err != nil {
			slog.Error("Configuration error", "error", err)
			return err
		}
		release, err := mbkp.AcquireLock(cfg.BackupDir)
		if err != nil {
			slog.Error("Cannot start incremental backup", "error", err)
			return err
		}
		defer func() { _ = release() }()
		if err := mbkp.RunIncrementalBackup(cmd.Context(), cfg, parentId); err != nil {
			slog.Error("Incremental backup failed", "error", err)
			return err
		}
		return nil
	},
}

var backupBinlogCmd = &cobra.Command{
	Use:   "binlog",
	Short: "Flush and archive MariaDB binary logs",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := mbkp.LoadConfig(backupDir)
		if err != nil {
			slog.Error("Configuration error", "error", err)
			return err
		}
		release, err := mbkp.AcquireLock(cfg.BackupDir)
		if err != nil {
			slog.Error("Cannot start binlog archiving", "error", err)
			return err
		}
		defer func() { _ = release() }()
		if err := mbkp.BackupBinlogs(cmd.Context(), cfg); err != nil {
			slog.Error("Binlog archiving failed", "error", err)
			return err
		}
		return nil
	},
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all backups",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := mbkp.LoadConfig(backupDir)
		if err != nil {
			slog.Error("Configuration error", "error", err)
			return err
		}
		if err := mbkp.ListBackups(cmd.Context(), cfg, outputFmt); err != nil {
			slog.Error("List failed", "error", err)
			return err
		}
		return nil
	},
}

var restoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Restore a backup to a MariaDB data directory",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := mbkp.LoadConfig(backupDir)
		if err != nil {
			slog.Error("Configuration error", "error", err)
			return err
		}
		if !prepareOnly && datadir == "" {
			fmt.Println("Error: --datadir is required unless --prepare-only is set.")
			_ = cmd.Usage()
			return fmt.Errorf("--datadir is required")
		}
		release, err := mbkp.AcquireLock(cfg.BackupDir)
		if err != nil {
			slog.Error("Cannot start restore", "error", err)
			return err
		}
		defer func() { _ = release() }()
		if err := mbkp.RestoreBackup(cmd.Context(), cfg, backupId, datadir, prepareOnly, newServerUUID); err != nil {
			slog.Error("Restore failed", "error", err)
			return err
		}
		return nil
	},
}

var pitrCmd = &cobra.Command{
	Use:   "pitr",
	Short: "Perform Point-in-Time Recovery (PITR) to a specific timestamp",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := mbkp.LoadConfig(backupDir)
		if err != nil {
			slog.Error("Configuration error", "error", err)
			return err
		}
		if targetTime == "" {
			fmt.Println("Error: --target-time is required.")
			_ = cmd.Usage()
			return fmt.Errorf("--target-time is required")
		}
		if datadir == "" {
			fmt.Println("Error: --datadir is required.")
			_ = cmd.Usage()
			return fmt.Errorf("--datadir is required")
		}
		parsedTime, err := parseTargetTime(targetTime)
		if err != nil {
			slog.Error("Error parsing target-time, must be in RFC3339 format (e.g. 2006-01-02T15:04:05Z or '2006-01-02T15:04:05+05:30') or 'YYYY-MM-DD HH:MM:SS' (interpreted in the local timezone)", "target_time", targetTime, "error", err)
			return err
		}
		release, err := mbkp.AcquireLock(cfg.BackupDir)
		if err != nil {
			slog.Error("Cannot start PITR", "error", err)
			return err
		}
		defer func() { _ = release() }()
		if err := mbkp.RunPITR(cmd.Context(), cfg, parsedTime, datadir, newServerUUID); err != nil {
			slog.Error("PITR failed", "error", err)
			return err
		}
		return nil
	},
}

// parseTargetTime parses a --target-time value, accepting RFC3339 (with an
// explicit UTC offset) or a zoneless "YYYY-MM-DD HH:MM:SS" interpreted in the
// local timezone, matching how mariadb-binlog interprets --stop-datetime.
func parseTargetTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.ParseInLocation("2006-01-02 15:04:05", s, time.Local)
}

var purgeCmd = &cobra.Command{
	Use:   "purge",
	Short: "Purge backups and archived binlogs based on a retention policy",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := mbkp.LoadConfig(backupDir)
		if err != nil {
			slog.Error("Configuration error", "error", err)
			return err
		}
		if retention == "" {
			fmt.Println("Error: --retention is required.")
			_ = cmd.Usage()
			return fmt.Errorf("--retention is required")
		}
		release, err := mbkp.AcquireLock(cfg.BackupDir)
		if err != nil {
			slog.Error("Cannot start purge", "error", err)
			return err
		}
		defer func() { _ = release() }()
		if err := mbkp.PurgeBackups(cmd.Context(), cfg, retention, dryRun); err != nil {
			slog.Error("Purge failed", "error", err)
			return err
		}
		return nil
	},
}

func init() {
	// Root flags
	rootCmd.PersistentFlags().StringVar(&backupDir, "backup-dir", "", "Directory to store and read backups (overrides MBKP_BACKUP_DIR)")

	// Backup subcommands
	backupCmd.AddCommand(backupFullCmd)
	backupCmd.AddCommand(backupIncrementalCmd)
	backupCmd.AddCommand(backupBinlogCmd)

	// Incremental flags
	backupIncrementalCmd.Flags().StringVar(&parentId, "parent-id", "", "Parent backup ID (default: latest completed backup)")

	// List flags
	listCmd.Flags().StringVar(&outputFmt, "output", "table", "Output format: table or json")

	// Restore flags
	restoreCmd.Flags().StringVar(&backupId, "backup-id", "", "Backup ID to restore (default: latest completed backup)")
	restoreCmd.Flags().StringVar(&datadir, "datadir", "", "Target MariaDB data directory")
	restoreCmd.Flags().BoolVar(&prepareOnly, "prepare-only", false, "Prepare the backup in place but do not copy-back")
	restoreCmd.Flags().BoolVar(&newServerUUID, "new-server-uuid", false, "Start the restored server with a freshly generated server-uuid instead of the one recorded in the backup (MySQL/Percona only; use when the restored server will run alongside the original, e.g. clones)")

	// PITR flags
	pitrCmd.Flags().StringVar(&targetTime, "target-time", "", "Target timestamp for recovery (RFC3339 format, e.g. '2026-06-09T14:30:00Z'; zoneless 'YYYY-MM-DD HH:MM:SS' is interpreted in the local timezone)")
	pitrCmd.Flags().StringVar(&datadir, "datadir", "", "Target MariaDB data directory")
	pitrCmd.Flags().BoolVar(&newServerUUID, "new-server-uuid", false, "Recover with a freshly generated server-uuid instead of the one recorded in the backup (MySQL/Percona only; use when the recovered server will run alongside the original, e.g. clones)")

	// Purge flags
	purgeCmd.Flags().StringVar(&retention, "retention", "", "Retention policy duration (e.g. '7d', '30d', '24h')")
	purgeCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be purged without deleting files/metadata")

	// Add commands to root
	rootCmd.AddCommand(backupCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(restoreCmd)
	rootCmd.AddCommand(pitrCmd)
	rootCmd.AddCommand(purgeCmd)

}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		// After the first signal initiates graceful shutdown, deregister the handler
		// so a second signal immediately terminates via default OS handling.
		stop()
	}()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			os.Exit(130)
		}
		os.Exit(1)
	}
}
