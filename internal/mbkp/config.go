package mbkp

import (
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/go-sql-driver/mysql"
	"github.com/spf13/viper"
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

func getEnv(key, defaultVal string) string {
	if val, exists := os.LookupEnv(key); exists {
		return val
	}
	return defaultVal
}

// GetDSN creates a data source name for the mysql driver
func (c *Config) GetDSN() (string, error) {
	var dsn string
	if c.Socket != "" {
		dsn = fmt.Sprintf("%s:%s@unix(%s)/", c.User, c.Password, c.Socket)
	} else {
		dsn = fmt.Sprintf("%s:%s@tcp(%s:%d)/", c.User, c.Password, c.Host, c.Port)
	}

	// Setup TLS if specified
	if c.TLSCA != "" || c.TLSCert != "" {
		tlsConfigName := "mbkp-tls"
		tlsConfig := &tls.Config{}

		if c.TLSCA != "" {
			caCert, err := os.ReadFile(c.TLSCA)
			if err != nil {
				return "", fmt.Errorf("failed to read TLS CA: %w", err)
			}
			caCertPool := x509.NewCertPool()
			caCertPool.AppendCertsFromPEM(caCert)
			tlsConfig.RootCAs = caCertPool
		}

		if c.TLSCert != "" && c.TLSKey != "" {
			cert, err := tls.LoadX509KeyPair(c.TLSCert, c.TLSKey)
			if err != nil {
				return "", fmt.Errorf("failed to load client TLS key pair: %w", err)
			}
			tlsConfig.Certificates = []tls.Certificate{cert}
		}

		tlsConfig.InsecureSkipVerify = !c.TLSVerify

		err := mysql.RegisterTLSConfig(tlsConfigName, tlsConfig)
		if err != nil {
			return "", fmt.Errorf("failed to register TLS config: %w", err)
		}

		dsn += "?tls=" + tlsConfigName
	}

	return dsn, nil
}

// ConnectDB establishes a connection to the database
func (c *Config) ConnectDB() (*sql.DB, error) {
	dsn, err := c.GetDSN()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database connection: %w", err)
	}
	if err := db.Ping(); err != nil {
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
		args = append(args, "--tls")
		if c.TLSCA != "" {
			args = append(args, "--tls-ca="+c.TLSCA)
		}
		if c.TLSCert != "" {
			args = append(args, "--tls-cert="+c.TLSCert)
		}
		if c.TLSKey != "" {
			args = append(args, "--tls-key="+c.TLSKey)
		}
		if !c.TLSVerify {
			// standard client options skip-ssl-verify
			// Note: mariabackup/mariadb support --tls-verify-server-cert or not
			// We can omit it or pass appropriate flags if needed.
		} else {
			args = append(args, "--tls-verify-server-cert")
		}
	}

	return args
}
