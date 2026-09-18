package mbkp

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/spf13/viper"
)

// Bounds applied to every database connection so a hung network path (a server
// that accepts the connection but never answers, a black-holed route) fails in
// bounded time instead of stalling the caller indefinitely. All queries mbkp
// runs over these connections (SHOW VARIABLES, FLUSH BINARY LOGS, SHOW BINARY
// LOGS) are fast metadata operations, so the read timeout never cuts a
// legitimately slow one short.
const (
	connectTimeout = 5 * time.Second
	readTimeout    = 30 * time.Second
)

// TLS material already registered with the mysql driver's process-global
// registry, keyed by the fingerprint-derived config name (see
// registerMySQLTLSConfig).
var (
	tlsRegisteredMu sync.Mutex
	tlsRegistered   = map[string]struct{}{}
)

type Config struct {
	Host      string
	Port      int
	User      string
	Password  string
	Socket    string
	TLSCA     string
	TLSCert   string
	TLSKey    string
	TLSVerify bool
	BackupDir string
	BackupBin string // resolved backup binary: "mariadb-backup", "mariabackup", or "xtrabackup"
	StreamBin string // resolved stream extract binary: "mbstream" (MariaDB) or "xbstream" (Percona/MySQL)
	BinlogBin string // resolved binlog replay binary: "mariadb-binlog" (MariaDB) or "mysqlbinlog" (MySQL)
	ClientBin string // resolved mysql client binary: "mariadb" (MariaDB) or "mysql" (MySQL/Percona)
}

// detectBackupTools resolves the correct binaries for backup, stream extraction,
// binlog replay, and the MySQL client from what is available on PATH.
// Detection order: mariadb-backup → mariabackup → xtrabackup.
// When MariaDB tooling is found the binlog/client binaries are probed
// individually; when xtrabackup is found MySQL tooling is assumed.
func detectBackupTools() (backupBin, streamBin, binlogBin, clientBin string) {
	// MariaDB 11.x (renamed binary)
	if _, err := exec.LookPath("mariadb-backup"); err == nil {
		slog.Info("Backup tool selected", "binary", "mariadb-backup")
		return "mariadb-backup", "mbstream", detectBinlogBin(), detectClientBin()
	}
	// MariaDB 10.x (legacy name)
	if _, err := exec.LookPath("mariabackup"); err == nil {
		slog.Info("Backup tool selected", "binary", "mariabackup")
		return "mariabackup", "mbstream", detectBinlogBin(), detectClientBin()
	}
	// Percona XtraBackup / Oracle MySQL
	if _, err := exec.LookPath("xtrabackup"); err == nil {
		slog.Info("Backup tool selected", "binary", "xtrabackup")
		return "xtrabackup", "xbstream", "mysqlbinlog", "mysql"
	}
	// Nothing found — return defaults and let the first invocation surface the error.
	slog.Warn("no backup tool found on PATH (tried mariadb-backup, mariabackup, xtrabackup); defaulting to mariadb-backup")
	return "mariadb-backup", "mbstream", "mariadb-binlog", "mariadb"
}

// detectBinlogBin returns the binlog replay tool available on PATH.
// Prefers the MariaDB-branded name when present.
func detectBinlogBin() string {
	if _, err := exec.LookPath("mariadb-binlog"); err == nil {
		return "mariadb-binlog"
	}
	if _, err := exec.LookPath("mysqlbinlog"); err == nil {
		return "mysqlbinlog"
	}
	return "mariadb-binlog"
}

// detectClientBin returns the MySQL/MariaDB command-line client available on PATH.
// Prefers the MariaDB-branded name when present.
func detectClientBin() string {
	if _, err := exec.LookPath("mariadb"); err == nil {
		return "mariadb"
	}
	if _, err := exec.LookPath("mysql"); err == nil {
		return "mysql"
	}
	return "mariadb"
}

func LoadConfig(backupDirFlag string) (*Config, error) {
	v := viper.New()

	// Bind environment variables explicitly to match fallback chain
	_ = v.BindEnv("host", "MARIADB_HOST", "MYSQL_HOST")
	_ = v.BindEnv("port", "MARIADB_PORT", "MYSQL_PORT")
	_ = v.BindEnv("user", "MARIADB_USER", "MYSQL_USER")
	_ = v.BindEnv("password", "MARIADB_PASSWORD", "MYSQL_PASSWORD", "MYSQL_PWD", "MARIADB_ROOT_PASSWORD", "MYSQL_ROOT_PASSWORD")
	_ = v.BindEnv("socket", "MARIADB_SOCKET", "MYSQL_UNIX_PORT")
	_ = v.BindEnv("tls_ca", "MARIADB_TLS_CA")
	_ = v.BindEnv("tls_cert", "MARIADB_TLS_CERT")
	_ = v.BindEnv("tls_key", "MARIADB_TLS_KEY")
	_ = v.BindEnv("tls_verify", "MARIADB_TLS_VERIFY")
	_ = v.BindEnv("backup_dir", "MBKP_BACKUP_DIR")

	// Set default values
	v.SetDefault("host", "localhost")
	v.SetDefault("port", 3306)
	v.SetDefault("user", "root")
	v.SetDefault("tls_verify", true)

	// If argument flag is provided, override backup_dir in viper
	if backupDirFlag != "" {
		v.Set("backup_dir", backupDirFlag)
	}

	backupDir := v.GetString("backup_dir")
	if backupDir == "" {
		return nil, fmt.Errorf("backup directory must be specified via --backup-dir or MBKP_BACKUP_DIR env var")
	}

	absBackupDir, err := filepath.Abs(backupDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve absolute path of backup directory: %w", err)
	}

	port := v.GetInt("port")
	if port <= 0 {
		port = 3306
	}

	tlsVerify := true
	if v.IsSet("tls_verify") {
		tlsVerify = v.GetBool("tls_verify")
	}

	backupBin, streamBin, binlogBin, clientBin := detectBackupTools()
	return &Config{
		Host:      v.GetString("host"),
		Port:      port,
		User:      v.GetString("user"),
		Password:  v.GetString("password"),
		Socket:    v.GetString("socket"),
		TLSCA:     v.GetString("tls_ca"),
		TLSCert:   v.GetString("tls_cert"),
		TLSKey:    v.GetString("tls_key"),
		TLSVerify: tlsVerify,
		BackupDir: absBackupDir,
		BackupBin: backupBin,
		StreamBin: streamBin,
		BinlogBin: binlogBin,
		ClientBin: clientBin,
	}, nil
}

// GetDSN creates a data source name for the mysql driver
func (c *Config) GetDSN() (string, error) {
	mc := mysql.NewConfig()
	mc.User = c.User
	mc.Passwd = c.Password
	mc.Timeout = connectTimeout
	mc.ReadTimeout = readTimeout
	if c.Socket != "" {
		mc.Net = "unix"
		mc.Addr = c.Socket
	} else {
		mc.Net = "tcp"
		mc.Addr = net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	}

	// Setup TLS if specified
	if c.TLSCA != "" || c.TLSCert != "" {
		tlsConfig := &tls.Config{}
		// The PEM bytes double as fingerprint input for the one-time
		// registration below, so read the key pair from memory rather than
		// through tls.LoadX509KeyPair's file paths.
		var caPEM, certPEM, keyPEM []byte

		if c.TLSCA != "" {
			var err error
			caPEM, err = os.ReadFile(c.TLSCA)
			if err != nil {
				return "", fmt.Errorf("failed to read TLS CA: %w", err)
			}
			caCertPool := x509.NewCertPool()
			if !caCertPool.AppendCertsFromPEM(caPEM) {
				return "", fmt.Errorf("no valid certificates parsed from TLS CA file %s", c.TLSCA)
			}
			tlsConfig.RootCAs = caCertPool
		}

		if c.TLSCert != "" && c.TLSKey != "" {
			var err error
			certPEM, err = os.ReadFile(c.TLSCert)
			if err != nil {
				return "", fmt.Errorf("failed to load client TLS key pair: %w", err)
			}
			keyPEM, err = os.ReadFile(c.TLSKey)
			if err != nil {
				return "", fmt.Errorf("failed to load client TLS key pair: %w", err)
			}
			cert, err := tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				return "", fmt.Errorf("failed to load client TLS key pair: %w", err)
			}
			tlsConfig.Certificates = []tls.Certificate{cert}
		}

		tlsConfig.InsecureSkipVerify = !c.TLSVerify

		tlsConfigName, err := registerMySQLTLSConfig(caPEM, certPEM, keyPEM, c.TLSVerify, tlsConfig)
		if err != nil {
			return "", err
		}

		mc.TLSConfig = tlsConfigName
	}

	return mc.FormatDSN(), nil
}

// registerMySQLTLSConfig registers tlsConfig with the mysql driver's
// process-global TLS registry at most once per unique TLS material. The
// registration name is derived from a SHA-256 fingerprint of the material, so
// repeated calls with identical settings (e.g. the PITR readiness loop's
// connect polls) reuse the existing registration instead of re-registering,
// and changed settings get a fresh name instead of silently overwriting the
// old one. Registrations are intentionally never deregistered: their lifetime
// is the process lifetime (the driver looks names up lazily on every dial,
// and a CLI has no point where pooled connections are guaranteed done), and
// the per-material names keep the registry from ever going stale.
func registerMySQLTLSConfig(caPEM, certPEM, keyPEM []byte, verify bool, tlsConfig *tls.Config) (string, error) {
	// Length-prefix each field so different material splits cannot collide.
	fp := sha256.New()
	for _, b := range [][]byte{caPEM, certPEM, keyPEM} {
		_, _ = fmt.Fprintf(fp, "%d\n", len(b))
		_, _ = fp.Write(b)
	}
	_, _ = fmt.Fprintf(fp, "verify=%t", verify)
	name := "mbkp-tls-" + hex.EncodeToString(fp.Sum(nil)[:8])

	tlsRegisteredMu.Lock()
	defer tlsRegisteredMu.Unlock()
	if _, ok := tlsRegistered[name]; ok {
		return name, nil
	}
	if err := mysql.RegisterTLSConfig(name, tlsConfig); err != nil {
		return "", fmt.Errorf("failed to register TLS config: %w", err)
	}
	tlsRegistered[name] = struct{}{}
	return name, nil
}

// ConnectDB establishes a connection to the database
func (c *Config) ConnectDB(ctx context.Context) (*sql.DB, error) {
	dsn, err := c.GetDSN()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database connection: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}
	return db, nil
}

// GetCommonArgs returns client flags for CLI tools (mariabackup, mariadb-binlog, mariadb)
func (c *Config) GetCommonArgs() []string {
	var args []string
	args = append(args, "--user="+c.User)

	if c.Socket != "" {
		args = append(args, "--socket="+c.Socket)
	} else {
		args = append(args, "--host="+c.Host, "--port="+strconv.Itoa(c.Port))
	}

	if c.TLSCA != "" || c.TLSCert != "" {
		args = append(args, "--ssl")
		if c.TLSCA != "" {
			args = append(args, "--ssl-ca="+c.TLSCA)
		}
		if c.TLSCert != "" {
			args = append(args, "--ssl-cert="+c.TLSCert)
		}
		if c.TLSKey != "" {
			args = append(args, "--ssl-key="+c.TLSKey)
		}
		if c.TLSVerify {
			args = append(args, "--ssl-verify-server-cert")
		}
	}

	return args
}
