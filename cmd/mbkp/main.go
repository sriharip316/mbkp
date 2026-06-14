package main

import (
	"fmt"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/srihari/mbkp/internal/mbkp"
)

var (
	backupDir   string
	parentId    string
	outputFmt   string
	backupId    string
	datadir     string
	prepareOnly bool
	targetTime  string
	retention   string
	dryRun      bool
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
	Run: func(cmd *cobra.Command, args []string) {
		_ = cmd.Help()
		os.Exit(1)
	},
}

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Perform a backup (full, incremental, or binlog)",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Error: backup subcommand (full, incremental, binlog) is required.")
		fmt.Println("Usage: mbkp backup <full | incremental | binlog>")
		os.Exit(1)
	},
}

var backupFullCmd = &cobra.Command{
	Use:   "full",
	Short: "Perform a full physical backup of MariaDB",
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := mbkp.LoadConfig(backupDir)
		if err != nil {
			slog.Error("Configuration error", "error", err)
			os.Exit(1)
		}
		if err := mbkp.RunFullBackup(cfg); err != nil {
			slog.Error("Full backup failed", "error", err)
			os.Exit(1)
		}
	},
}

var backupIncrementalCmd = &cobra.Command{
	Use:   "incremental",
	Short: "Perform an incremental physical backup",
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := mbkp.LoadConfig(backupDir)
		if err != nil {
			slog.Error("Configuration error", "error", err)
			os.Exit(1)
		}
		if err := mbkp.RunIncrementalBackup(cfg, parentId); err != nil {
			slog.Error("Incremental backup failed", "error", err)
			os.Exit(1)
		}
	},
}

var backupBinlogCmd = &cobra.Command{
	Use:   "binlog",
	Short: "Flush and archive MariaDB binary logs",
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := mbkp.LoadConfig(backupDir)
		if err != nil {
			slog.Error("Configuration error", "error", err)
			os.Exit(1)
		}
		if err := mbkp.BackupBinlogs(cfg); err != nil {
			slog.Error("Binlog archiving failed", "error", err)
			os.Exit(1)
		}
	},
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all backups",
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := mbkp.LoadConfig(backupDir)
		if err != nil {
			slog.Error("Configuration error", "error", err)
			os.Exit(1)
		}
		if err := mbkp.ListBackups(cfg, outputFmt); err != nil {
			slog.Error("List failed", "error", err)
			os.Exit(1)
		}
	},
}

var restoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Restore a backup to a MariaDB data directory",
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := mbkp.LoadConfig(backupDir)
		if err != nil {
			slog.Error("Configuration error", "error", err)
			os.Exit(1)
		}
		if !prepareOnly && datadir == "" {
			fmt.Println("Error: --datadir is required unless --prepare-only is set.")
			_ = cmd.Usage()
			os.Exit(1)
		}
		if err := mbkp.RestoreBackup(cfg, backupId, datadir, prepareOnly); err != nil {
			slog.Error("Restore failed", "error", err)
			os.Exit(1)
		}
	},
}

var pitrCmd = &cobra.Command{
	Use:   "pitr",
	Short: "Perform Point-in-Time Recovery (PITR) to a specific timestamp",
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := mbkp.LoadConfig(backupDir)
		if err != nil {
			slog.Error("Configuration error", "error", err)
			os.Exit(1)
		}
		if targetTime == "" {
			fmt.Println("Error: --target-time is required.")
			_ = cmd.Usage()
			os.Exit(1)
		}
		if datadir == "" {
			fmt.Println("Error: --datadir is required.")
			_ = cmd.Usage()
			os.Exit(1)
		}
		parsedTime, err := time.Parse(time.RFC3339, targetTime)
		if err != nil {
			parsedTime, err = time.Parse("2006-01-02 15:04:05", targetTime)
			if err != nil {
				slog.Error("Error parsing target-time, must be in RFC3339 format (e.g. 2006-01-02T15:04:05Z or 2006-01-02T15:04:05+05:30) or 'YYYY-MM-DD HH:MM:SS'", "target_time", targetTime)
				os.Exit(1)
			}
		}
		if err := mbkp.RunPITR(cfg, parsedTime, datadir); err != nil {
			slog.Error("PITR failed", "error", err)
			os.Exit(1)
		}
	},
}

var purgeCmd = &cobra.Command{
	Use:   "purge",
	Short: "Purge backups and archived binlogs based on a retention policy",
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := mbkp.LoadConfig(backupDir)
		if err != nil {
			slog.Error("Configuration error", "error", err)
			os.Exit(1)
		}
		if retention == "" {
			fmt.Println("Error: --retention is required.")
			_ = cmd.Usage()
			os.Exit(1)
		}
		if err := mbkp.PurgeBackups(cfg, retention, dryRun); err != nil {
			slog.Error("Purge failed", "error", err)
			os.Exit(1)
		}
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

	// PITR flags
	pitrCmd.Flags().StringVar(&targetTime, "target-time", "", "Target timestamp for recovery (RFC3339 format, e.g. '2026-06-09T14:30:00Z')")
	pitrCmd.Flags().StringVar(&datadir, "datadir", "", "Target MariaDB data directory")

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
	log.SetFlags(log.LstdFlags)
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
