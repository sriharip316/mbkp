package main

import (
	"testing"
	"time"
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
