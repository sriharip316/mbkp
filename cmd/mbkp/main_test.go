package main

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/srihari/mbkp/internal/mbkp"
)

func TestParseTargetTimeRFC3339(t *testing.T) {
	got, err := parseTargetTime("2026-06-09T14:30:00+05:30")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2026, time.June, 9, 14, 30, 0, 0, time.FixedZone("", 5*3600+30*60))
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if _, offset := got.Zone(); offset != 5*3600+30*60 {
		t.Errorf("utc offset = %d, want %d", offset, 5*3600+30*60)
	}
}

func TestParseTargetTimeZonelessUsesLocal(t *testing.T) {
	got, err := parseTargetTime("2026-06-09 14:30:00")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2026, time.June, 9, 14, 30, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got.Location() != time.Local {
		t.Errorf("location = %v, want time.Local", got.Location())
	}
}

func TestParseTargetTimeInvalid(t *testing.T) {
	for _, s := range []string{"", "not-a-time", "2026-06-09"} {
		if _, err := parseTargetTime(s); err == nil {
			t.Errorf("parseTargetTime(%q) succeeded, want error", s)
		}
	}
}

func TestRootCmdExecutionWithCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tmpDir := t.TempDir()
	rootCmd.SetArgs([]string{"--backup-dir", tmpDir, "backup", "full"})

	err := rootCmd.ExecuteContext(ctx)
	if err == nil {
		t.Fatal("expected ExecuteContext with canceled context to return error")
	}

	// Verify lock was released and can be acquired immediately
	release, err := mbkp.AcquireLock(tmpDir)
	if err != nil {
		t.Fatalf("expected lock to be released after cancellation, but failed to acquire: %v", err)
	}
	_ = release()
}

func TestBackupCmdRequiresSubcommand(t *testing.T) {
	rootCmd.SetArgs([]string{"backup"})
	err := rootCmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected 'mbkp backup' without subcommand to return error")
	}
}

func TestShutdownExitCode(t *testing.T) {
	// No recorded signal (e.g. the context was canceled by another path):
	// the SIGINT convention is the fallback.
	empty := make(chan os.Signal, 1)
	if got := shutdownExitCode(empty); got != 130 {
		t.Errorf("shutdownExitCode(empty) = %d, want 130", got)
	}

	intCh := make(chan os.Signal, 1)
	intCh <- os.Interrupt
	if got := shutdownExitCode(intCh); got != 130 {
		t.Errorf("shutdownExitCode(SIGINT) = %d, want 130", got)
	}

	termCh := make(chan os.Signal, 1)
	termCh <- syscall.SIGTERM
	if got := shutdownExitCode(termCh); got != 143 {
		t.Errorf("shutdownExitCode(SIGTERM) = %d, want 143", got)
	}
}
