package mbkp

import (
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

func TestGetEnv(t *testing.T) {
	t.Setenv("TEST_ENV_VAR", "value")
	if getEnv("TEST_ENV_VAR", "default") != "value" {
		t.Error("expected value")
	}
	if getEnv("NON_EXISTENT_VAR", "default") != "default" {
		t.Error("expected default")
	}
}

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
	expected := "root:pw@tcp(127.0.0.1:3306)/"
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
	expectedSocket := "root:pw@unix(/tmp/mysql.sock)/"
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
	expectedIPv6 := "root:pw@tcp([::1]:3306)/"
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
		t.Errorf("expected DSN to contain ?tls=mbkp-tls, got %q", dsn)
	}
}

func TestConnectDBError(t *testing.T) {
	cfg := &Config{
		User:     "root",
		Password: "pw",
		Host:     "127.0.0.1",
		Port:     1, // reserved port, connection will fail
	}
	_, err := cfg.ConnectDB()
	if err == nil {
		t.Error("expected error when trying to connect to an invalid port")
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
