package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

const (
	dbUser     = "root"
	dbPassword = "testpass"
	dbHost     = "localhost"
)

// testVariant describes one database image + tooling combination to exercise.
type testVariant struct {
	// Image is the fully-qualified container image reference.
	Image string
	// Version is used as the t.Run sub-test name (e.g. "MariaDB_11_4").
	Version string
	// RootPassEnv is the environment variable that sets the root DB password
	// when the container starts (MARIADB_ROOT_PASSWORD or MYSQL_ROOT_PASSWORD).
	RootPassEnv string
	// HostPort is the unique TCP port exposed on the host for this variant so
	// parallel subtests do not collide.
	HostPort string
	// Setup is an optional function called after a container starts but before
	// any backup commands run.  Use it to install extra tooling (e.g. xtrabackup)
	// that is not pre-baked into the image.  It receives the running container name.
	Setup func(t *testing.T, containerName string)
}

// testVariants is the full matrix of database images exercised by the E2E suite.
// Adding a new version is a one-line addition here.
var testVariants = []testVariant{
	// ── MariaDB ────────────────────────────────────────────────────────────────
	{
		Image:       "docker.io/library/mariadb:10.11",
		Version:     "MariaDB_10_11",
		RootPassEnv: "MARIADB_ROOT_PASSWORD",
		HostPort:    "13311",
	},
	{
		Image:       "docker.io/library/mariadb:11.4",
		Version:     "MariaDB_11_4",
		RootPassEnv: "MARIADB_ROOT_PASSWORD",
		HostPort:    "13314",
	},
	{
		Image:       "docker.io/library/mariadb:11.8",
		Version:     "MariaDB_11_8",
		RootPassEnv: "MARIADB_ROOT_PASSWORD",
		HostPort:    "13318",
	},
	// ── Percona Server (MySQL-compatible) ─────────────────────────────────────
	{
		Image:       "docker.io/percona/percona-server:8.4",
		Version:     "Percona_8_4",
		RootPassEnv: "MYSQL_ROOT_PASSWORD",
		HostPort:    "13384",
		Setup: func(t *testing.T, ctrName string) {
			t.Helper()
			installXtrabackup(t, ctrName, "percona-xtrabackup-84")
		},
	},
	{
		Image:       "docker.io/percona/percona-server:8.0",
		Version:     "Percona_8_0",
		RootPassEnv: "MYSQL_ROOT_PASSWORD",
		HostPort:    "13380",
		Setup: func(t *testing.T, ctrName string) {
			t.Helper()
			installXtrabackup(t, ctrName, "percona-xtrabackup-80")
		},
	},
}

// installXtrabackup installs a percona-xtrabackup package inside a running
// container using microdnf and a manually-fetched libev RPM package, as the
// official Percona Server images are Red Hat Enterprise Linux-based and do
// not include libev in the standard UBI subscription profiles.
func installXtrabackup(t *testing.T, ctrName, pkg string) {
	t.Helper()
	t.Logf("Installing %s in container %s…", pkg, ctrName)

	// UBI 9 doesn't include libev in its repositories, and EPEL relies on CRB which
	// is also restricted. We download the libev RPM directly from Rocky Linux 9 BaseOS.
	libevURL := "https://dl.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os/Packages/l/libev-4.33-6.el9.x86_64.rpm"
	t.Logf("Downloading and installing libev dependency RPM…")
	if _, err := runCmd("podman", "exec", "--user", "root", ctrName,
		"curl", "-sSL", "-o", "/tmp/libev.rpm", libevURL); err != nil {
		t.Fatalf("Failed to download libev RPM in %s: %v", ctrName, err)
	}
	if _, err := runCmd("podman", "exec", "--user", "root", ctrName,
		"rpm", "-i", "/tmp/libev.rpm"); err != nil {
		// Non-fatal if already installed.
		t.Logf("rpm -i libev.rpm warning (non-fatal): %v", err)
	}

	// Enable the Percona Tools repository.
	t.Logf("Enabling Percona Tools repository…")
	if _, err := runCmd("podman", "exec", "--user", "root", ctrName,
		"percona-release", "enable-only", "tools", "release"); err != nil {
		t.Fatalf("Failed to enable Percona Tools repo in %s: %v", ctrName, err)
	}

	// Install the requested percona-xtrabackup package and perl-English (required by xtrabackup).
	t.Logf("Installing %s package and perl-English…", pkg)
	if _, err := runCmd("podman", "exec", "--user", "root", ctrName,
		"microdnf", "install", "-y", pkg, "perl-English"); err != nil {
		t.Fatalf("Failed to install %s in %s: %v", pkg, ctrName, err)
	}
	t.Logf("%s and perl-English installed successfully in %s", pkg, ctrName)
}

func runCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("command %s %s failed: %w. Output: %s",
			name, strings.Join(args, " "), err, string(out))
	}
	return string(out), nil
}

func waitForDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	var db *sql.DB
	var err error

	// Poll for up to 60 seconds
	for i := range 30 {
		db, err = sql.Open("mysql", dsn)
		if err == nil {
			err = db.Ping()
			if err == nil {
				return db
			}
		}
		if db != nil {
			_ = db.Close()
		}
		t.Logf("Waiting for DB to be ready… (%d/30)", i+1)
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("DB failed to start: %v", err)
	return nil
}

// TestE2EBackupRestoreAndPITR runs the full backup → restore → PITR → purge
// workflow for every variant in testVariants.
// By default, all variants run in parallel. Set MBKP_E2E_SEQUENTIAL=1 to run
// them sequentially (useful for debugging or resource-constrained environments).
func TestE2EBackupRestoreAndPITR(t *testing.T) {
	sequential := os.Getenv("MBKP_E2E_SEQUENTIAL") == "1"
	if sequential {
		t.Logf("Running tests sequentially (MBKP_E2E_SEQUENTIAL=1)")
	}

	for _, v := range testVariants {
		v := v // capture for parallel closure
		t.Run(v.Version, func(t *testing.T) {
			if !sequential {
				t.Parallel()
			}
			runE2EForVariant(t, v)
		})
	}
}

// runE2EForVariant executes the complete E2E scenario for one testVariant.
// All Podman resources (containers, volumes, binary) are scoped to the variant
// so that parallel runs cannot collide.
func runE2EForVariant(t *testing.T, v testVariant) {
	t.Helper()

	// Derive unique Podman resource names from the variant version string.
	suffix := strings.ToLower(strings.ReplaceAll(v.Version, "_", "-"))
	ctrSource := "mbkp-src-" + suffix
	ctrRecovery := "mbkp-rec-" + suffix
	volData := "mbkp-data-" + suffix
	volBackups := "mbkp-backups-" + suffix
	binaryPath := "mbkp-" + suffix // version-scoped binary to avoid write-races

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/", dbUser, dbPassword, dbHost, v.HostPort)

	// ── 1. Clean up any previous test remnants ────────────────────────────────
	t.Logf("[%s] Cleaning up old containers and volumes…", v.Version)
	_, _ = runCmd("podman", "rm", "-f", ctrSource)
	_, _ = runCmd("podman", "rm", "-f", ctrRecovery)
	_, _ = runCmd("podman", "volume", "rm", "-f", volData)
	_, _ = runCmd("podman", "volume", "rm", "-f", volBackups)

	// ── 2. Create Podman volumes ──────────────────────────────────────────────
	t.Logf("[%s] Creating Podman volumes…", v.Version)
	if _, err := runCmd("podman", "volume", "create", volData); err != nil {
		t.Fatalf("Failed to create data volume: %v", err)
	}
	if _, err := runCmd("podman", "volume", "create", volBackups); err != nil {
		t.Fatalf("Failed to create backups volume: %v", err)
	}

	defer func() {
		t.Logf("[%s] Cleaning up volumes and containers…", v.Version)
		_, _ = runCmd("podman", "rm", "-f", ctrSource)
		_, _ = runCmd("podman", "rm", "-f", ctrRecovery)
		_, _ = runCmd("podman", "volume", "rm", "-f", volData)
		_, _ = runCmd("podman", "volume", "rm", "-f", volBackups)
	}()

	// ── 3. Start source database container ───────────────────────────────────
	t.Logf("[%s] Starting source container (%s)…", v.Version, v.Image)
	if _, err := runCmd("podman", "run", "--name", ctrSource, "-d",
		"--memory=512m",
		"-v", volData+":/var/lib/mysql",
		"-v", volBackups+":/backups",
		"-p", v.HostPort+":3306",
		"-e", v.RootPassEnv+"="+dbPassword,
		v.Image,
		"--log-bin=binlog",
		"--server-id=1",
	); err != nil {
		t.Fatalf("Failed to start source container: %v", err)
	}

	// ── 4. Optional per-variant setup (e.g. install xtrabackup) ─────────────
	if v.Setup != nil {
		v.Setup(t, ctrSource)
	}

	// ── 5. Wait for DB connection ─────────────────────────────────────────────
	db := waitForDB(t, dsn)
	defer func() { _ = db.Close() }()

	// ── 6. Create test schema and seed initial data ───────────────────────────
	t.Logf("[%s] Creating test schema and seeding initial data…", v.Version)
	if _, err := db.Exec("CREATE DATABASE testdb"); err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE testdb.t1 (id INT AUTO_INCREMENT PRIMARY KEY, val VARCHAR(50), created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)"); err != nil {
		t.Fatalf("Failed to create table: %v", err)
	}
	if _, err := db.Exec("INSERT INTO testdb.t1 (val) VALUES ('initial_data')"); err != nil {
		t.Fatalf("Failed to insert initial data: %v", err)
	}

	// ── 7. Build the mbkp binary for Linux/amd64 ─────────────────────────────
	t.Logf("[%s] Compiling mbkp for Linux…", v.Version)
	cmdBuild := exec.Command("go", "build", "-o", binaryPath, ".")
	cmdBuild.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := cmdBuild.CombinedOutput(); err != nil {
		t.Fatalf("Failed to build mbkp: %v. Output: %s", err, string(out))
	}
	defer func() { _ = os.Remove(binaryPath) }()

	// ── 8. Copy mbkp to source container ─────────────────────────────────────
	t.Logf("[%s] Copying mbkp binary into source container…", v.Version)
	if _, err := runCmd("podman", "cp", binaryPath, ctrSource+":/usr/local/bin/mbkp"); err != nil {
		t.Fatalf("Failed to copy binary: %v", err)
	}

	// ── 9. Run Full Backup ────────────────────────────────────────────────────
	t.Logf("[%s] Executing full backup…", v.Version)
	if _, err := runCmd("podman", "exec",
		"-e", "MBKP_BACKUP_DIR=/backups",
		"-e", "MARIADB_PASSWORD="+dbPassword,
		ctrSource, "mbkp", "backup", "full",
	); err != nil {
		t.Fatalf("Full backup failed: %v", err)
	}
	if out, err := runCmd("podman", "exec", ctrSource, "find", "/backups", "-type", "f"); err == nil {
		t.Logf("[%s] Files in /backups after full backup:\n%s", v.Version, out)
	}

	// ── 10. Insert incremental data ───────────────────────────────────────────
	t.Logf("[%s] Inserting incremental data…", v.Version)
	if _, err := db.Exec("INSERT INTO testdb.t1 (val) VALUES ('incremental_data')"); err != nil {
		t.Fatalf("Failed to insert incremental data: %v", err)
	}

	// ── 11. Run Incremental Backup ────────────────────────────────────────────
	t.Logf("[%s] Executing incremental backup…", v.Version)
	if _, err := runCmd("podman", "exec",
		"-e", "MBKP_BACKUP_DIR=/backups",
		"-e", "MARIADB_PASSWORD="+dbPassword,
		ctrSource, "mbkp", "backup", "incremental",
	); err != nil {
		t.Fatalf("Incremental backup failed: %v", err)
	}
	if out, err := runCmd("podman", "exec", ctrSource, "find", "/backups", "-type", "f"); err == nil {
		t.Logf("[%s] Files in /backups after incremental backup:\n%s", v.Version, out)
	}

	// ── 12. Insert PITR data 1 ────────────────────────────────────────────────
	t.Logf("[%s] Inserting PITR data 1…", v.Version)
	if _, err := db.Exec("INSERT INTO testdb.t1 (val) VALUES ('pitr_data_1')"); err != nil {
		t.Fatalf("Failed to insert PITR data 1: %v", err)
	}

	// Sleep so pitr_data_1 has a strictly earlier timestamp than the recovery time.
	time.Sleep(2 * time.Second)

	// ── 13. Capture PITR target timestamp ────────────────────────────────────
	var recoveryTimeStr string
	if err := db.QueryRow("SELECT NOW()").Scan(&recoveryTimeStr); err != nil {
		t.Fatalf("Failed to get DB server time: %v", err)
	}
	t.Logf("[%s] PITR target recovery timestamp: %s", v.Version, recoveryTimeStr)

	time.Sleep(2 * time.Second)

	// ── 14. Insert PITR data 2 (must NOT appear after recovery) ──────────────
	t.Logf("[%s] Inserting PITR data 2…", v.Version)
	if _, err := db.Exec("INSERT INTO testdb.t1 (val) VALUES ('pitr_data_2')"); err != nil {
		t.Fatalf("Failed to insert PITR data 2: %v", err)
	}

	// ── 15. Archive binary logs ───────────────────────────────────────────────
	t.Logf("[%s] Archiving binary logs…", v.Version)
	if _, err := runCmd("podman", "exec",
		"-e", "MBKP_BACKUP_DIR=/backups",
		"-e", "MARIADB_PASSWORD="+dbPassword,
		ctrSource, "mbkp", "backup", "binlog",
	); err != nil {
		t.Fatalf("Binlog archiving failed: %v", err)
	}

	// ── 16. Simulate disaster ─────────────────────────────────────────────────
	t.Logf("[%s] Simulating disaster (dropping table testdb.t1)…", v.Version)
	if _, err := db.Exec("DROP TABLE testdb.t1"); err != nil {
		t.Fatalf("Failed to drop table: %v", err)
	}
	_ = db.Close()

	// ==========================================================================
	// TEST CASE A: PHYSICAL RESTORE (to incremental backup)
	// ==========================================================================
	t.Logf("[%s] === Testing physical Restore to Incremental Backup ===", v.Version)
	_, _ = runCmd("podman", "rm", "-f", ctrSource)

	t.Logf("[%s] Starting recovery container…", v.Version)
	if _, err := runCmd("podman", "run", "--name", ctrRecovery, "-d",
		"--memory=512m",
		"-v", volData+":/var/lib/mysql",
		"-v", volBackups+":/backups",
		v.Image, "sleep", "1000",
	); err != nil {
		t.Fatalf("Failed to start recovery container: %v", err)
	}

	// Install tooling in recovery container if needed.
	if v.Setup != nil {
		v.Setup(t, ctrRecovery)
	}

	t.Logf("[%s] Copying mbkp to recovery container…", v.Version)
	if _, err := runCmd("podman", "cp", binaryPath, ctrRecovery+":/usr/local/bin/mbkp"); err != nil {
		t.Fatalf("Failed to copy binary to recovery container: %v", err)
	}

	t.Logf("[%s] Emptying data directory…", v.Version)
	if _, err := runCmd("podman", "exec", ctrRecovery, "sh", "-c",
		"find /var/lib/mysql -mindepth 1 -delete"); err != nil {
		t.Fatalf("Failed to empty datadir: %v", err)
	}

	t.Logf("[%s] Running mbkp restore…", v.Version)
	if _, err := runCmd("podman", "exec",
		"-e", "MBKP_BACKUP_DIR=/backups",
		"-e", "MARIADB_PASSWORD="+dbPassword,
		ctrRecovery, "mbkp", "restore", "--datadir=/var/lib/mysql",
	); err != nil {
		t.Fatalf("Restore failed: %v", err)
	}

	_, _ = runCmd("podman", "rm", "-f", ctrRecovery)

	t.Logf("[%s] Restarting source container with restored files…", v.Version)
	if _, err := runCmd("podman", "run", "--name", ctrSource, "-d",
		"--memory=512m",
		"-v", volData+":/var/lib/mysql",
		"-v", volBackups+":/backups",
		"-p", v.HostPort+":3306",
		"-e", v.RootPassEnv+"="+dbPassword,
		v.Image,
		"--log-bin=mysql-bin",
		"--binlog-format=ROW",
		"--server-id=1",
	); err != nil {
		t.Fatalf("Failed to restart source container: %v", err)
	}

	db = waitForDB(t, dsn)

	t.Logf("[%s] Verifying restored data…", v.Version)
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM testdb.t1").Scan(&count); err != nil {
		t.Fatalf("Failed to query restored table: %v", err)
	}
	if count != 2 {
		t.Fatalf("Expected 2 rows (initial_data, incremental_data), got %d", count)
	}

	rows, err := db.Query("SELECT val FROM testdb.t1 ORDER BY id")
	if err != nil {
		t.Fatalf("Failed to query row values: %v", err)
	}
	var vals []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("Failed to scan row: %v", err)
		}
		vals = append(vals, v)
	}
	_ = rows.Close()

	if len(vals) != 2 || vals[0] != "initial_data" || vals[1] != "incremental_data" {
		t.Fatalf("Unexpected row values in restored table: %v", vals)
	}
	t.Logf("[%s] Physical restore verified successfully!", v.Version)

	// ==========================================================================
	// TEST CASE B: POINT-IN-TIME RECOVERY (PITR)
	// ==========================================================================
	t.Logf("[%s] === Testing Point-in-Time Recovery (PITR) ===", v.Version)
	_ = db.Close()
	_, _ = runCmd("podman", "rm", "-f", ctrSource)

	t.Logf("[%s] Starting PITR recovery container…", v.Version)
	if _, err := runCmd("podman", "run", "--name", ctrRecovery, "-d",
		"--memory=512m",
		"-v", volData+":/var/lib/mysql",
		"-v", volBackups+":/backups",
		v.Image, "sleep", "1000",
	); err != nil {
		t.Fatalf("Failed to start PITR recovery container: %v", err)
	}

	// Install tooling in PITR recovery container if needed.
	if v.Setup != nil {
		v.Setup(t, ctrRecovery)
	}

	t.Logf("[%s] Copying mbkp to PITR recovery container…", v.Version)
	if _, err := runCmd("podman", "cp", binaryPath, ctrRecovery+":/usr/local/bin/mbkp"); err != nil {
		t.Fatalf("Failed to copy binary to PITR recovery container: %v", err)
	}

	t.Logf("[%s] Emptying data directory…", v.Version)
	if _, err := runCmd("podman", "exec", ctrRecovery, "sh", "-c",
		"find /var/lib/mysql -mindepth 1 -delete"); err != nil {
		t.Fatalf("Failed to empty datadir for PITR: %v", err)
	}

	t.Logf("[%s] Running mbkp pitr…", v.Version)
	pitrOut, err := runCmd("podman", "exec",
		"-e", "MBKP_BACKUP_DIR=/backups",
		"-e", "MARIADB_PASSWORD="+dbPassword,
		ctrRecovery, "mbkp", "pitr",
		"--target-time="+recoveryTimeStr,
		"--datadir=/var/lib/mysql",
	)
	if err != nil {
		logContent, _ := runCmd("podman", "exec", ctrRecovery, "cat", "/backups/pitr_mariadbd.log")
		t.Logf("=== pitr_mariadbd.log ===\n%s", logContent)
		t.Fatalf("PITR failed: %v. Output: %s", err, pitrOut)
	}
	t.Logf("[%s] PITR output:\n%s", v.Version, pitrOut)

	_, _ = runCmd("podman", "rm", "-f", ctrRecovery)

	t.Logf("[%s] Restarting source container with PITR-recovered files…", v.Version)
	if _, err := runCmd("podman", "run", "--name", ctrSource, "-d",
		"--memory=512m",
		"-v", volData+":/var/lib/mysql",
		"-v", volBackups+":/backups",
		"-p", v.HostPort+":3306",
		"-e", v.RootPassEnv+"="+dbPassword,
		v.Image,
		"--log-bin=mysql-bin",
		"--binlog-format=ROW",
		"--server-id=1",
	); err != nil {
		t.Fatalf("Failed to restart source container after PITR: %v", err)
	}

	db = waitForDB(t, dsn)

	t.Logf("[%s] Verifying PITR recovered data…", v.Version)
	if err := db.QueryRow("SELECT COUNT(*) FROM testdb.t1").Scan(&count); err != nil {
		t.Fatalf("Failed to query PITR table: %v", err)
	}
	if count != 3 {
		t.Fatalf("Expected 3 rows (initial_data, incremental_data, pitr_data_1), got %d", count)
	}

	rows, err = db.Query("SELECT val FROM testdb.t1 ORDER BY id")
	if err != nil {
		t.Fatalf("Failed to query PITR row values: %v", err)
	}
	vals = nil
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("Failed to scan PITR row: %v", err)
		}
		vals = append(vals, v)
	}
	_ = rows.Close()

	expectedVals := []string{"initial_data", "incremental_data", "pitr_data_1"}
	for i, val := range vals {
		if val != expectedVals[i] {
			t.Fatalf("Expected row %d value %q, got %q", i, expectedVals[i], val)
		}
	}
	t.Logf("[%s] PITR recovery verified! All 3 rows present, pitr_data_2 correctly absent.", v.Version)

	// ==========================================================================
	// TEST CASE C: PURGE
	// ==========================================================================
	t.Logf("[%s] === Testing Purge Old Backups ===", v.Version)
	time.Sleep(2 * time.Second) // ensure backups are older than 1 s

	t.Logf("[%s] Copying mbkp binary into container for purge test…", v.Version)
	if _, err := runCmd("podman", "cp", binaryPath, ctrSource+":/usr/local/bin/mbkp"); err != nil {
		t.Fatalf("Failed to copy binary for purge: %v", err)
	}

	t.Logf("[%s] Running mbkp purge --dry-run…", v.Version)
	dryRunOut, err := runCmd("podman", "exec",
		"-e", "MBKP_BACKUP_DIR=/backups",
		"-e", "MARIADB_PASSWORD="+dbPassword,
		ctrSource, "mbkp", "purge", "--retention=1s", "--dry-run",
	)
	if err != nil {
		t.Fatalf("Dry-run purge failed: %v", err)
	}
	t.Logf("[%s] Dry-run purge output:\n%s", v.Version, dryRunOut)
	if !strings.Contains(dryRunOut, "Would purge backup") {
		t.Fatalf("Dry-run output did not contain 'Would purge backup': %s", dryRunOut)
	}

	t.Logf("[%s] Running actual mbkp purge…", v.Version)
	purgeOut, err := runCmd("podman", "exec",
		"-e", "MBKP_BACKUP_DIR=/backups",
		"-e", "MARIADB_PASSWORD="+dbPassword,
		ctrSource, "mbkp", "purge", "--retention=1s",
	)
	if err != nil {
		t.Fatalf("Actual purge failed: %v", err)
	}
	t.Logf("[%s] Actual purge output:\n%s", v.Version, purgeOut)

	listOut, err := runCmd("podman", "exec",
		"-e", "MBKP_BACKUP_DIR=/backups",
		ctrSource, "mbkp", "list",
	)
	if err != nil {
		t.Fatalf("List command failed: %v", err)
	}
	t.Logf("[%s] List output after purge:\n%s", v.Version, listOut)
	if strings.Contains(listOut, "full_") || strings.Contains(listOut, "inc_") {
		t.Fatalf("Backups not fully purged; list still shows backups: %s", listOut)
	}

	findOut, err := runCmd("podman", "exec", ctrSource, "find", "/backups", "-name", "*.xbstream*")
	if err == nil && strings.TrimSpace(findOut) != "" {
		t.Fatalf("Physical archive files not deleted:\n%s", findOut)
	}
	t.Logf("[%s] Purge E2E verified successfully!", v.Version)
}
