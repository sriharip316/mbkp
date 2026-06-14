# mbkp - MariaDB Backup & Recovery Tool

[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**mbkp** is a powerful command-line tool for managing MariaDB physical backups, restores, and Point-in-Time Recovery (PITR). It provides automated incremental backups, binary log archiving, and intelligent retention policies with dependency-aware purging.

## ✨ Features

- **Full & Incremental Backups**: Efficient physical backups using `mariabackup`, `mariadb-backup`, or `xtrabackup`
- **Binary Log Archiving**: Automated archival of MariaDB binary logs for PITR
- **Point-in-Time Recovery (PITR)**: Restore your database to any specific timestamp
- **Intelligent Retention**: Dependency-aware backup purging that preserves restoration chains
- **Compression Support**: Automatic LZ4 or GZIP compression for optimal storage efficiency
- **Metadata Tracking**: SQLite-based backup catalog for fast queries and lineage tracking
- **Multi-Tool Support**: Works with MariaDB 10.x/11.x (`mariabackup`/`mariadb-backup`) and Percona XtraBackup
- **Dry-Run Mode**: Preview purge operations before executing
- **Flexible Output**: Table or JSON format for backup listings

## 📋 Table of Contents

- [Installation](#-installation)
- [Prerequisites](#-prerequisites)
- [Quick Start](#-quick-start)
- [Usage](#-usage)
  - [Backup Operations](#backup-operations)
  - [Restore Operations](#restore-operations)
  - [Point-in-Time Recovery](#point-in-time-recovery)
  - [Listing Backups](#listing-backups)
  - [Purging Old Backups](#purging-old-backups)
- [Configuration](#-configuration)
- [Architecture](#-architecture)
- [Examples](#-examples)
- [Demo](#-demo)
- [Development](#-development)
- [License](#-license)

## 🚀 Installation

### From GitHub Release

Download the latest release from the [GitHub Releases page](https://github.com/sriharip316/mbkp/releases/latest).

### From Source

```bash
# Clone the repository
git clone https://github.com/srihari/mbkp.git
cd mbkp

# Build the binary
make build

# Install to GOPATH/bin
make install
```

### Using Go Install

```bash
go install github.com/srihari/mbkp/cmd/mbkp@latest
```

## 📦 Prerequisites

### Required Tools

Depending on your MariaDB/MySQL variant, ensure one of the following backup tool sets is installed:

**MariaDB 11.x:**
- `mariadb-backup` (backup tool)
- `mbstream` (stream handler)
- `mariadb-binlog` (binlog processor)
- `mariadb` (client)

**MariaDB 10.x:**
- `mariabackup` (backup tool)
- `mbstream` (stream handler)
- `mariadb-binlog` or `mysqlbinlog` (binlog processor)
- `mariadb` or `mysql` (client)

**Percona/MySQL:**
- `xtrabackup` (backup tool)
- `xbstream` (stream handler)
- `mysqlbinlog` (binlog processor)
- `mysql` (client)

### Compression Tools (Optional)

At least one of the following (LZ4 is preferred for performance):
- `lz4` - Fast compression (recommended)
- `gzip` - Fallback compression

### Database Configuration

Enable binary logging in your MariaDB configuration:

```ini
[mysqld]
log_bin = /var/log/mysql/binlog
binlog_format = ROW
server_id = 1
```

## 🏃 Quick Start

```bash
# Set up environment variables
export MARIADB_HOST=localhost
export MARIADB_USER=root
export MARIADB_PASSWORD=your_password
export MBKP_BACKUP_DIR=/path/to/backups

# Create a full backup
mbkp backup full

# Create an incremental backup
mbkp backup incremental

# Archive binary logs
mbkp backup binlog

# List all backups
mbkp list

# Restore latest backup
mbkp restore --datadir=/path/to/mysql/data

# Perform Point-in-Time Recovery
mbkp pitr --target-time="2026-06-14T10:30:00Z" --datadir=/path/to/mysql/data

# Purge old backups (keep last 7 days)
mbkp purge --retention=7d
```

## 📖 Usage

### Backup Operations

#### Full Backup

Creates a complete physical backup of your MariaDB database:

```bash
mbkp backup full --backup-dir=/backups
```

#### Incremental Backup

Creates an incremental backup containing only changes since the last backup:

```bash
# Use latest backup as parent (automatic)
mbkp backup incremental

# Specify parent backup ID explicitly
mbkp backup incremental --parent-id=BACKUP_ID
```

#### Binary Log Archiving

Flushes and archives binary logs for PITR:

```bash
mbkp backup binlog
```

**Best Practice**: Run this command regularly (e.g., every hour via cron) to ensure you can recover to any point in time.

### Restore Operations

#### Basic Restore

Restore the latest backup to a target data directory:

```bash
mbkp restore --datadir=/var/lib/mysql
```

#### Restore Specific Backup

```bash
mbkp restore --backup-id=20260614_103000 --datadir=/var/lib/mysql
```

#### Prepare-Only Mode

Prepare the backup without copying it to the data directory (useful for validation):

```bash
mbkp restore --backup-id=20260614_103000 --prepare-only
```

**Important**: 
- The target `--datadir` must be empty
- Stop MariaDB before running restore
- Ensure proper file ownership after restore

### Point-in-Time Recovery

Restore your database to a specific timestamp by automatically finding the appropriate backup and replaying binary logs:

```bash
# RFC3339 format
mbkp pitr --target-time="2026-06-14T10:30:00Z" --datadir=/var/lib/mysql

# Simple datetime format
mbkp pitr --target-time="2026-06-14 10:30:00" --datadir=/var/lib/mysql
```

**How PITR Works**:
1. Finds the closest backup completed before the target time
2. Restores that backup
3. Starts a temporary MariaDB instance
4. Replays archived binary logs up to the target timestamp
5. Cleanly stops the temporary instance

### Listing Backups

#### Table Format (Default)

```bash
mbkp list
```

#### JSON Format

```bash
mbkp list --output=json
```

### Purging Old Backups

Remove old backups while maintaining restoration chains:

```bash
# Preview what would be deleted (dry-run)
mbkp purge --retention=7d --dry-run

# Actually purge backups older than 7 days
mbkp purge --retention=7d

# Keep 30 days of backups
mbkp purge --retention=30d
```

**Retention Format**: `<number><unit>` where unit is:
- `h` - hours
- `d` - days
- `w` - weeks (7d)
- `m` - months (30d)

**Smart Purging**:
- Preserves parent backups needed for incremental restoration chains
- Retains binary logs required by kept backups
- Cleans up metadata for missing archive files

## ⚙️ Configuration

### Environment Variables

`mbkp` uses environment variables for database connection and configuration. Variables are checked in fallback order:

#### Connection Settings

| Variable | MariaDB Variant | MySQL Variant | Default | Description |
|----------|----------------|---------------|---------|-------------|
| Host | `MARIADB_HOST` | `MYSQL_HOST` | `localhost` | Database server hostname |
| Port | `MARIADB_PORT` | `MYSQL_PORT` | `3306` | Database server port |
| User | `MARIADB_USER` | `MYSQL_USER` | `root` | Database username |
| Password | `MARIADB_PASSWORD` | `MYSQL_PASSWORD` | - | Database password |
| | | `MYSQL_PWD` | - | (fallback) |
| | `MARIADB_ROOT_PASSWORD` | `MYSQL_ROOT_PASSWORD` | - | (fallback) |
| Socket | `MARIADB_SOCKET` | `MYSQL_UNIX_PORT` | - | Unix socket path |

#### TLS/SSL Settings

| Variable | Description |
|----------|-------------|
| `MARIADB_SSL_CA` | Path to CA certificate |
| `MARIADB_SSL_CERT` | Path to client certificate |
| `MARIADB_SSL_KEY` | Path to client key |
| `MARIADB_SSL_VERIFY` | Verify SSL (default: `true`) |

#### Backup Settings

| Variable | Description |
|----------|-------------|
| `MBKP_BACKUP_DIR` | Directory for storing backups (can be overridden by `--backup-dir` flag) |

### Command-Line Flags

Global flag available on all commands:

```bash
--backup-dir string    Directory to store and read backups
```

See `mbkp <command> --help` for command-specific flags.

## 🏗️ Architecture

### Directory Structure

```
<backup-dir>/
├── backups.db                       # SQLite metadata catalog
├── full_YYYYMMDD_HHMMSS.xbstream.gz # Full backup archives
├── full_YYYYMMDD_HHMMSS.xbstream.lz4
├── inc_YYYYMMDD_HHMMSS.xbstream.gz  # Incremental backup archives
├── inc_YYYYMMDD_HHMMSS.xbstream.lz4
├── binlogs/                         # Archived binary logs
│   ├── binlog.000001.gz
│   ├── binlog.000001.lz4
│   ├── binlog.000002.gz
│   └── ...
└── lsn/                             # LSN checkpoint tracking
    ├── xtrabackup_checkpoints       # LSN position data
    └── xtrabackup_info              # Backup metadata
```

**Naming Convention**:
- Full backups: `full_YYYYMMDD_HHMMSS.xbstream.<ext>`
- Incremental backups: `inc_YYYYMMDD_HHMMSS.xbstream.<ext>`
- Binary logs: `<binlog_filename>.<ext>`
- Extension: `.lz4` (preferred) or `.gz` (fallback)

### Metadata Schema

Backups are tracked in an SQLite database (`backups.db`) with the following schema:

```sql
CREATE TABLE backups (
    id          TEXT PRIMARY KEY,        -- Timestamp-based backup ID
    type        TEXT NOT NULL,           -- 'full' or 'incremental'
    status      TEXT NOT NULL,           -- 'in_progress', 'completed', 'failed'
    start_time  TEXT NOT NULL,           -- ISO8601 UTC timestamp
    end_time    TEXT,                    -- ISO8601 UTC timestamp (nullable)
    path        TEXT NOT NULL,           -- Relative path to archive file
    binlog_file TEXT,                    -- Active binlog at backup completion
    binlog_pos  INTEGER DEFAULT 0,       -- Binlog position
    parent_id   TEXT                     -- Parent backup ID (NULL for full)
);
```

### Backup Tool Detection

`mbkp` automatically detects available backup tools at startup:

1. **Backup Binary**: Prefers `mariadb-backup` → `mariabackup` → `xtrabackup`
2. **Stream Binary**: Uses `mbstream` (MariaDB) or `xbstream` (Percona)
3. **Binlog Binary**: Uses `mariadb-binlog` (MariaDB) or `mysqlbinlog` (MySQL/Percona)
4. **Client Binary**: Uses `mariadb` (MariaDB) or `mysql` (MySQL/Percona)

### Compression Pipeline

Backups are compressed on-the-fly during creation:

```
mariabackup --stream=xbstream | lz4 > backup.xbstream.lz4
```

**Compression Selection**:
- **LZ4**: Preferred (fastest, lower CPU usage)
- **GZIP**: Fallback if LZ4 is unavailable

### Incremental Backup Chain

Incremental backups form a dependency chain:

```
Full Backup (F1)
└── Incremental 1 (I1) → parent: F1
    └── Incremental 2 (I2) → parent: I1
        └── Incremental 3 (I3) → parent: I2
```

**Restoration** requires applying the entire chain: F1 → I1 → I2 → I3

**LSN Tracking**: All incremental backups reference a shared LSN directory for efficient chaining.

## 📚 Examples

### Automated Backup Schedule (Cron)

```bash
# Daily full backup at 2 AM
0 2 * * * /usr/local/bin/mbkp backup full --backup-dir=/backups

# Hourly incremental backups
0 * * * * /usr/local/bin/mbkp backup incremental --backup-dir=/backups

# Hourly binlog archival
15 * * * * /usr/local/bin/mbkp backup binlog --backup-dir=/backups

# Weekly cleanup (retain 30 days)
0 3 * * 0 /usr/local/bin/mbkp purge --retention=30d --backup-dir=/backups
```

### Recovery Scenarios

#### Scenario 1: Full Database Crash

```bash
# Stop MariaDB
systemctl stop mariadb

# Clear data directory
rm -rf /var/lib/mysql/*

# Restore latest backup
mbkp restore --datadir=/var/lib/mysql --backup-dir=/backups

# Fix ownership
chown -R mysql:mysql /var/lib/mysql

# Start MariaDB
systemctl start mariadb
```

#### Scenario 2: Accidental Data Deletion

```bash
# User dropped table at 2026-06-14 10:45:00
# Need to restore to 10:44:50 (10 seconds before)

systemctl stop mariadb
rm -rf /var/lib/mysql/*

mbkp pitr \
  --target-time="2026-06-14T10:44:50Z" \
  --datadir=/var/lib/mysql \
  --backup-dir=/backups

chown -R mysql:mysql /var/lib/mysql
systemctl start mariadb
```

#### Scenario 3: Testing Backup Validity

```bash
# Prepare backup without copying to production
mbkp restore \
  --backup-id=20260614_020000 \
  --prepare-only \
  --backup-dir=/backups

# Check the prepared backup in the temporary directory
# (location will be shown in output)
```

### Docker/Podman Example

```bash
# Backup containerized MariaDB
docker exec mariadb-container \
  mbkp backup full --backup-dir=/backups

# Restore to new container
docker run -d \
  --name mariadb-restored \
  -v /host/backups:/backups \
  -e MARIADB_ROOT_PASSWORD=password \
  mariadb:latest

docker exec mariadb-restored \
  mbkp restore --datadir=/var/lib/mysql --backup-dir=/backups
```

## 🎬 Demo

Want to see `mbkp` in action? Run the interactive demo script:

```bash
./demo.sh
```

This automated demo creates an isolated MariaDB container and walks through:
- Creating full, incremental, and binlog backups
- Listing and purging backups with retention policies
- Restoring full and incremental backups
- Performing Point-in-Time Recovery (PITR)
- Validating data integrity throughout

Perfect for quickly understanding how `mbkp` works without affecting your production environment!

## 🛠️ Development

### Building from Source

```bash
# Clone repository
git clone https://github.com/srihari/mbkp.git
cd mbkp

# Install dependencies
go mod download

# Build binary
make build

# Run tests
make test

# Run linters
make lint

# Generate coverage report
make cover
```

### Makefile Targets

| Target | Description |
|--------|-------------|
| `make build` | Build the CLI binary (local OS/ARCH) |
| `make install` | Install to GOPATH/bin |
| `make test` | Run unit tests |
| `make lint` | Run linters (golangci-lint, go vet, gofmt, staticcheck) |
| `make cover` | Run tests with coverage report |
| `make cover-html` | Generate HTML coverage report |
| `make clean` | Remove build artifacts |
| `make ci` | Run full CI pipeline (tidy, lint, test, cover) |
| `make release` | Build release binaries for multiple platforms |
| `make tag` | Create and push git tag |

### Running Integration Tests

The project includes comprehensive end-to-end tests using Podman/Docker:

```bash
cd cmd/mbkp
go test -v -run TestE2E
```

### Interactive Demo Script

A complete demo script is available to showcase all `mbkp` features in an isolated Podman container:

```bash
./demo.sh
```

**What the demo does**:
1. Creates a MariaDB 10.11 container with binary logging enabled
2. Sets up a test database and table with auto-incrementing data
3. Demonstrates full, incremental, and binlog backups
4. Tests backup listing (table and JSON formats)
5. Validates backup purging with retention policies
6. Tests full backup restoration
7. Tests incremental backup restoration
8. Tests Point-in-Time Recovery (PITR)
9. Verifies data integrity after each restore operation

**Requirements**:
- `podman` installed and running
- `pwgen` for generating random passwords
- Sufficient disk space for test backups

The demo runs completely automated and validates all operations, making it ideal for:
- Verifying installation and setup
- Understanding the backup/restore workflow
- Testing changes during development
- Demonstrating capabilities to stakeholders

### Project Structure

```
mbkp/
├── cmd/
│   └── mbkp/              # CLI entry point
│       ├── main.go        # Command definitions
│       └── e2e_test.go    # Integration tests
├── internal/
│   └── mbkp/              # Core library
│       ├── backup.go      # Backup operations
│       ├── restore.go     # Restore operations
│       ├── pitr.go        # Point-in-time recovery
│       ├── binlog.go      # Binary log archival
│       ├── purge.go       # Retention & cleanup
│       ├── metadata.go    # SQLite catalog
│       ├── list.go        # Backup listing
│       └── config.go      # Configuration loading
├── Makefile               # Build automation
├── go.mod                 # Go module definition
├── AGENTS.md              # AI developer guide
└── README.md              # This file
```

## 📄 License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.

## 🤝 Contributing

Contributions are welcome! Please feel free to submit issues, feature requests, or pull requests.

### Development Guidelines

1. **Read the [AGENTS.md](AGENTS.md)** file for architectural details
2. Run `make ci` before submitting PRs
3. Update documentation when adding/changing features
4. Maintain backward compatibility for metadata schema
5. Add tests for new functionality

## 🙏 Acknowledgments

- Built with [Cobra](https://github.com/spf13/cobra) for CLI framework
- Uses [Viper](https://github.com/spf13/viper) for configuration management
- Powered by [MariaBackup](https://mariadb.com/kb/en/mariabackup-overview/) and [Percona XtraBackup](https://www.percona.com/software/mysql-database/percona-xtrabackup)

## 📞 Support

For issues, questions, or feature requests, please:
- Open an issue on [GitHub](https://github.com/srihari/mbkp/issues)
- Check existing documentation in [AGENTS.md](AGENTS.md)

---

**Made with ❤️ for the MariaDB community**
