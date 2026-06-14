package mbkp

import (
	"testing"
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
}

func TestGetDSNTLSErrors(t *testing.T) {
	cfg := &Config{
		TLSCA: "non-existent-ca.pem",
	}
	_, err := cfg.GetDSN()
	if err == nil {
		t.Error("expected error when TLS CA file does not exist")
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

	// Test with TLS arguments
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
		"--tls",
		"--tls-ca=ca.pem",
		"--tls-cert=cert.pem",
		"--tls-key=key.pem",
		"--tls-verify-server-cert",
	}
	if len(argsTLS) != len(expectedTLS) {
		t.Fatalf("expected %v, got %v", expectedTLS, argsTLS)
	}
	for i, v := range argsTLS {
		if v != expectedTLS[i] {
			t.Errorf("at index %d: expected %q, got %q", i, expectedTLS[i], v)
		}
	}
}
