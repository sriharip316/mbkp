package mbkp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestLoadConfig(t *testing.T) {
	t.Setenv("MARIADB_HOST", "myhost")
	t.Setenv("MARIADB_PORT", "1234")
	t.Setenv("MARIADB_USER", "myuser")
	t.Setenv("MARIADB_PASSWORD", "mypass")
	t.Setenv("MBKP_BACKUP_DIR", "/tmp/backup")

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Host != "myhost" {
		t.Errorf("expected host myhost, got %s", cfg.Host)
	}
	if cfg.Port != 1234 {
		t.Errorf("expected port 1234, got %d", cfg.Port)
	}
	if cfg.User != "myuser" {
		t.Errorf("expected user myuser, got %s", cfg.User)
	}
	if cfg.Password != "mypass" {
		t.Errorf("expected password mypass, got %s", cfg.Password)
	}

	// Overriding backup dir with flag
	cfg2, err := LoadConfig("/tmp/backup_flag")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg2.BackupDir != "/tmp/backup_flag" {
		t.Errorf("expected backup dir /tmp/backup_flag, got %s", cfg2.BackupDir)
	}

	// Missing backup dir error
	t.Setenv("MBKP_BACKUP_DIR", "")
	_, err = LoadConfig("")
	if err == nil {
		t.Error("expected error when backup dir is missing")
	}
}

func TestGetDSN(t *testing.T) {
	cfg := &Config{
		User:     "root",
		Password: "pw",
		Host:     "127.0.0.1",
		Port:     3306,
	}
	dsn, err := cfg.GetDSN()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := "root:pw@tcp(127.0.0.1:3306)/?readTimeout=30s&timeout=5s"
	if dsn != expected {
		t.Errorf("expected %q, got %q", expected, dsn)
	}

	cfgSocket := &Config{
		User:     "root",
		Password: "pw",
		Socket:   "/tmp/mysql.sock",
	}
	dsnSocket, err := cfgSocket.GetDSN()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedSocket := "root:pw@unix(/tmp/mysql.sock)/?readTimeout=30s&timeout=5s"
	if dsnSocket != expectedSocket {
		t.Errorf("expected %q, got %q", expectedSocket, dsnSocket)
	}

	// IPv6 host
	cfgIPv6 := &Config{
		User:     "root",
		Password: "pw",
		Host:     "::1",
		Port:     3306,
	}
	dsnIPv6, err := cfgIPv6.GetDSN()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedIPv6 := "root:pw@tcp([::1]:3306)/?readTimeout=30s&timeout=5s"
	if dsnIPv6 != expectedIPv6 {
		t.Errorf("expected %q, got %q", expectedIPv6, dsnIPv6)
	}

	// Passwords with metacharacters that would break fmt.Sprintf DSN parsing
	specialPasswords := []string{
		"p@ss",
		"p:ss",
		"p/ss",
		"p)ss",
		`p\ss`,
		"p?ss",
		"p&ss",
		`p@s:s/w)d\?&`,
	}
	for _, p := range specialPasswords {
		cfgSpecial := &Config{
			User:     "root",
			Password: p,
			Host:     "127.0.0.1",
			Port:     3306,
		}
		dsnSpecial, err := cfgSpecial.GetDSN()
		if err != nil {
			t.Fatalf("unexpected error for password %q: %v", p, err)
		}
		parsed, err := mysql.ParseDSN(dsnSpecial)
		if err != nil {
			t.Fatalf("mysql.ParseDSN failed on %q for input password %q: %v", dsnSpecial, p, err)
		}
		if parsed.Passwd != p {
			t.Errorf("password mismatch: got %q, want %q", parsed.Passwd, p)
		}
		if parsed.User != "root" {
			t.Errorf("user mismatch: got %q, want %q", parsed.User, "root")
		}
		if parsed.Addr != "127.0.0.1:3306" {
			t.Errorf("addr mismatch: got %q, want %q", parsed.Addr, "127.0.0.1:3306")
		}
		if parsed.Net != "tcp" {
			t.Errorf("net mismatch: got %q, want %q", parsed.Net, "tcp")
		}
	}
}

func generateTestCACertPEM(t *testing.T) []byte {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test CA"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(1 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
}

func TestGetDSNTLSErrors(t *testing.T) {
	cfg := &Config{
		TLSCA: "non-existent-ca.pem",
	}
	_, err := cfg.GetDSN()
	if err == nil {
		t.Error("expected error when TLS CA file does not exist")
	}

	// Invalid/empty PEM file
	tmpDir := t.TempDir()
	invalidCAFile := filepath.Join(tmpDir, "invalid-ca.pem")
	if err := os.WriteFile(invalidCAFile, []byte("not-a-valid-certificate-pem"), 0600); err != nil {
		t.Fatalf("failed to write invalid CA file: %v", err)
	}
	cfgInvalid := &Config{
		TLSCA: invalidCAFile,
	}
	_, err = cfgInvalid.GetDSN()
	if err == nil {
		t.Error("expected error when TLS CA file contains invalid PEM data")
	} else if !strings.Contains(err.Error(), "no valid certificates parsed from TLS CA file") {
		t.Errorf("unexpected error message: %v", err)
	}

	emptyCAFile := filepath.Join(tmpDir, "empty-ca.pem")
	if err := os.WriteFile(emptyCAFile, []byte{}, 0600); err != nil {
		t.Fatalf("failed to write empty CA file: %v", err)
	}
	cfgEmpty := &Config{
		TLSCA: emptyCAFile,
	}
	_, err = cfgEmpty.GetDSN()
	if err == nil {
		t.Error("expected error when TLS CA file is empty")
	} else if !strings.Contains(err.Error(), "no valid certificates parsed from TLS CA file") {
		t.Errorf("unexpected error message: %v", err)
	}

	cfgCert := &Config{
		TLSCert: "non-existent-cert.pem",
		TLSKey:  "non-existent-key.pem",
	}
	_, err = cfgCert.GetDSN()
	if err == nil {
		t.Error("expected error when TLS cert/key files do not exist")
	}
}

func TestGetDSNTLSSuccess(t *testing.T) {
	// Keep the process-global TLS registry clean for other tests in the package.
	t.Cleanup(func() {
		tlsRegisteredMu.Lock()
		defer tlsRegisteredMu.Unlock()
		for name := range tlsRegistered {
			mysql.DeregisterTLSConfig(name)
		}
		tlsRegistered = map[string]struct{}{}
	})

	tmpDir := t.TempDir()
	validCAFile := filepath.Join(tmpDir, "valid-ca.pem")
	caPEM := generateTestCACertPEM(t)
	if err := os.WriteFile(validCAFile, caPEM, 0600); err != nil {
		t.Fatalf("failed to write valid CA file: %v", err)
	}

	cfg := &Config{
		User:      "root",
		Password:  "pw",
		Host:      "127.0.0.1",
		Port:      3306,
		TLSCA:     validCAFile,
		TLSVerify: true,
	}
	dsn, err := cfg.GetDSN()
	if err != nil {
		t.Fatalf("unexpected error with valid CA: %v", err)
	}
	if !strings.Contains(dsn, "tls=mbkp-tls") {
		t.Errorf("expected DSN to contain ?tls=mbkp-tls..., got %q", dsn)
	}

	// Repeated calls must reuse the single registration instead of
	// re-registering: identical DSN, exactly one registry entry.
	dsnRepeat, err := cfg.GetDSN()
	if err != nil {
		t.Fatalf("unexpected error on repeated call: %v", err)
	}
	if dsnRepeat != dsn {
		t.Errorf("expected identical DSN across repeated calls, got %q and %q", dsn, dsnRepeat)
	}
	if got := len(tlsRegistered); got != 1 {
		t.Errorf("expected exactly 1 registered TLS config after repeated calls, got %d", got)
	}

	// Different TLS material must register under a distinct name instead of
	// silently reusing (or overwriting) the previous registration.
	otherCAFile := filepath.Join(tmpDir, "other-ca.pem")
	if err := os.WriteFile(otherCAFile, generateTestCACertPEM(t), 0600); err != nil {
		t.Fatalf("failed to write other CA file: %v", err)
	}
	cfgOther := &Config{
		User:     "root",
		Password: "pw",
		Host:     "127.0.0.1",
		Port:     3306,
		TLSCA:    otherCAFile,
	}
	dsnOther, err := cfgOther.GetDSN()
	if err != nil {
		t.Fatalf("unexpected error with other CA: %v", err)
	}
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("failed to parse DSN %q: %v", dsn, err)
	}
	parsedOther, err := mysql.ParseDSN(dsnOther)
	if err != nil {
		t.Fatalf("failed to parse DSN %q: %v", dsnOther, err)
	}
	if parsed.TLSConfig == parsedOther.TLSConfig {
		t.Errorf("expected distinct TLS config names for distinct CA material, both are %q", parsed.TLSConfig)
	}
	if got := len(tlsRegistered); got != 2 {
		t.Errorf("expected exactly 2 registered TLS configs for distinct material, got %d", got)
	}
}

func TestConnectDBError(t *testing.T) {
	cfg := &Config{
		User:     "root",
		Password: "pw",
		Host:     "127.0.0.1",
		Port:     1, // reserved port, connection will fail
	}
	_, err := cfg.ConnectDB(context.Background())
	if err == nil {
		t.Error("expected error when trying to connect to an invalid port")
	}
}

func TestConnectDBCanceledContext(t *testing.T) {
	cfg := &Config{
		User:     "root",
		Password: "pw",
		Host:     "127.0.0.1",
		Port:     3306,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel context
	_, err := cfg.ConnectDB(ctx)
	if err == nil {
		t.Fatal("expected error with pre-canceled context")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("expected error to mention context canceled, got: %v", err)
	}
}

func TestGetCommonArgs(t *testing.T) {
	cfg := &Config{
		User:     "user1",
		Password: "pwd",
		Host:     "dbhost",
		Port:     3307,
	}
	args := cfg.GetCommonArgs()
	expected := []string{"--user=user1", "--host=dbhost", "--port=3307"}
	if len(args) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, args)
	}
	for i, v := range args {
		if v != expected[i] {
			t.Errorf("at index %d: expected %q, got %q", i, expected[i], v)
		}
	}

	cfgSocket := &Config{
		User:     "user1",
		Password: "pwd",
		Socket:   "/tmp/mysql.sock",
	}
	argsSocket := cfgSocket.GetCommonArgs()
	expectedSocket := []string{"--user=user1", "--socket=/tmp/mysql.sock"}
	if len(argsSocket) != len(expectedSocket) {
		t.Fatalf("expected %v, got %v", expectedSocket, argsSocket)
	}
	for i, v := range argsSocket {
		if v != expectedSocket[i] {
			t.Errorf("at index %d: expected %q, got %q", i, expectedSocket[i], v)
		}
	}

	// Test with TLS arguments and TLSVerify: true
	cfgTLS := &Config{
		User:      "user1",
		Password:  "pwd",
		Host:      "dbhost",
		Port:      3307,
		TLSCA:     "ca.pem",
		TLSCert:   "cert.pem",
		TLSKey:    "key.pem",
		TLSVerify: true,
	}
	argsTLS := cfgTLS.GetCommonArgs()
	expectedTLS := []string{
		"--user=user1",
		"--host=dbhost",
		"--port=3307",
		"--ssl",
		"--ssl-ca=ca.pem",
		"--ssl-cert=cert.pem",
		"--ssl-key=key.pem",
		"--ssl-verify-server-cert",
	}
	if len(argsTLS) != len(expectedTLS) {
		t.Fatalf("expected %v, got %v", expectedTLS, argsTLS)
	}
	for i, v := range argsTLS {
		if v != expectedTLS[i] {
			t.Errorf("at index %d: expected %q, got %q", i, expectedTLS[i], v)
		}
	}

	// Test with TLS arguments and TLSVerify: false
	cfgTLSNoVerify := &Config{
		User:      "user1",
		Password:  "pwd",
		Host:      "dbhost",
		Port:      3307,
		TLSCA:     "ca.pem",
		TLSCert:   "cert.pem",
		TLSKey:    "key.pem",
		TLSVerify: false,
	}
	argsTLSNoVerify := cfgTLSNoVerify.GetCommonArgs()
	expectedTLSNoVerify := []string{
		"--user=user1",
		"--host=dbhost",
		"--port=3307",
		"--ssl",
		"--ssl-ca=ca.pem",
		"--ssl-cert=cert.pem",
		"--ssl-key=key.pem",
	}
	if len(argsTLSNoVerify) != len(expectedTLSNoVerify) {
		t.Fatalf("expected %v, got %v", expectedTLSNoVerify, argsTLSNoVerify)
	}
	for i, v := range argsTLSNoVerify {
		if v != expectedTLSNoVerify[i] {
			t.Errorf("at index %d: expected %q, got %q", i, expectedTLSNoVerify[i], v)
		}
	}

	// Test with only TLSCA
	cfgTLSCAOnly := &Config{
		User:      "user1",
		Password:  "pwd",
		Host:      "dbhost",
		Port:      3307,
		TLSCA:     "ca.pem",
		TLSVerify: true,
	}
	argsTLSCAOnly := cfgTLSCAOnly.GetCommonArgs()
	expectedTLSCAOnly := []string{
		"--user=user1",
		"--host=dbhost",
		"--port=3307",
		"--ssl",
		"--ssl-ca=ca.pem",
		"--ssl-verify-server-cert",
	}
	if len(argsTLSCAOnly) != len(expectedTLSCAOnly) {
		t.Fatalf("expected %v, got %v", expectedTLSCAOnly, argsTLSCAOnly)
	}
	for i, v := range argsTLSCAOnly {
		if v != expectedTLSCAOnly[i] {
			t.Errorf("at index %d: expected %q, got %q", i, expectedTLSCAOnly[i], v)
		}
	}
}

func TestLoadConfig_PortValidation(t *testing.T) {
	t.Setenv("MBKP_BACKUP_DIR", t.TempDir())
	t.Setenv("MARIADB_PORT", "70000")

	_, err := LoadConfig("")
	if err == nil {
		t.Fatal("expected error for port > 65535, got nil")
	}
	if !strings.Contains(err.Error(), "invalid port") {
		t.Errorf("expected error message to mention 'invalid port', got %v", err)
	}
}

func TestLoadConfig_TLSEnvAliases(t *testing.T) {
	t.Setenv("MBKP_BACKUP_DIR", t.TempDir())
	// Neutralize any ambient TLS/SSL settings so the cases below start clean;
	// viper treats an empty-but-set variable as unset (allowEmptyEnv=false).
	for _, name := range []string{
		"MARIADB_TLS_CA", "MARIADB_TLS_CERT", "MARIADB_TLS_KEY", "MARIADB_TLS_VERIFY",
		"MARIADB_SSL_CA", "MARIADB_SSL_CERT", "MARIADB_SSL_KEY", "MARIADB_SSL_VERIFY",
	} {
		t.Setenv(name, "")
	}

	// SSL-only: the legacy alias must reach the config on its own.
	t.Setenv("MARIADB_SSL_CA", "/tmp/ssl-ca.pem")
	t.Setenv("MARIADB_SSL_CERT", "/tmp/ssl-cert.pem")
	t.Setenv("MARIADB_SSL_KEY", "/tmp/ssl-key.pem")
	t.Setenv("MARIADB_SSL_VERIFY", "false")
	cfgSSL, err := LoadConfig("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfgSSL.TLSCA != "/tmp/ssl-ca.pem" || cfgSSL.TLSCert != "/tmp/ssl-cert.pem" || cfgSSL.TLSKey != "/tmp/ssl-key.pem" {
		t.Errorf("expected MARIADB_SSL_* alias values, got ca=%q cert=%q key=%q", cfgSSL.TLSCA, cfgSSL.TLSCert, cfgSSL.TLSKey)
	}
	if cfgSSL.TLSVerify {
		t.Errorf("expected MARIADB_SSL_VERIFY=false to disable TLS verification")
	}

	// Both spellings set: the TLS name must win.
	t.Setenv("MARIADB_TLS_CA", "/tmp/tls-ca.pem")
	t.Setenv("MARIADB_TLS_VERIFY", "true")
	cfgBoth, err := LoadConfig("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfgBoth.TLSCA != "/tmp/tls-ca.pem" {
		t.Errorf("expected MARIADB_TLS_CA to take precedence, got %q", cfgBoth.TLSCA)
	}
	if !cfgBoth.TLSVerify {
		t.Errorf("expected MARIADB_TLS_VERIFY to take precedence over MARIADB_SSL_VERIFY")
	}
}
