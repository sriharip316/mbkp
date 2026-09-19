package mbkp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBinlogBaseName(t *testing.T) {
	cases := map[string]string{
		"binlog.000001":          "binlog.000001",
		"binlog.000001.lz4":      "binlog.000001",
		"binlog.000001.gz":       "binlog.000001",
		"mariadb-bin.000042.gz":  "mariadb-bin.000042",
		"binlog.000001.lz4.part": "binlog.000001.lz4.part", // partial archives keep their suffix
		"notes.txt":              "notes.txt",
	}
	for in, want := range cases {
		if got := binlogBaseName(in); got != want {
			t.Errorf("binlogBaseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGetBinlogFilesToApplyCompressedArchives(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"binlog.000001.lz4",
		"binlog.000002.lz4",
		"binlog.000003.gz",
		"binlog.000004.lz4.part", // partial archive must be ignored
		"notes.txt",              // not a binlog
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Directories matching the binlog pattern must be ignored.
	if err := os.Mkdir(filepath.Join(dir, "binlog.000009.gz"), 0755); err != nil {
		t.Fatal(err)
	}

	got, err := getBinlogFilesToApply(dir, "binlog.000002")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{
		filepath.Join(dir, "binlog.000002.lz4"),
		filepath.Join(dir, "binlog.000003.gz"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestGetBinlogFilesToApplyGapBeforeStartFile(t *testing.T) {
	dir := t.TempDir()
	// Only a later binlog exists; the start file itself is missing. The filter
	// still returns the later file, so RunPITR's start-file presence check is
	// what must reject this gap.
	if err := os.WriteFile(filepath.Join(dir, "binlog.000003.lz4"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := getBinlogFilesToApply(dir, "binlog.000002")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{filepath.Join(dir, "binlog.000003.lz4")}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestGetBinlogFilesToApplyDeduplicatesVariants guards against replaying a
// binlog twice when the archive holds two compression variants of the same
// file (e.g. a gzip run followed by an lz4 run): the second variant would
// decompress into the same output path and be handed to the binlog tool twice.
func TestGetBinlogFilesToApplyDeduplicatesVariants(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"binlog.000001.lz4",
		"binlog.000001.gz", // duplicate variant of the same binlog
		"binlog.000002.lz4",
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := getBinlogFilesToApply(dir, "binlog.000001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("expected one entry per binlog, got %d: %v", len(got), got)
	}
	bases := make(map[string]int)
	for _, p := range got {
		bases[binlogBaseName(filepath.Base(p))]++
	}
	for base, n := range bases {
		if n != 1 {
			t.Errorf("binlog %s appears %d times, want 1", base, n)
		}
	}
}
