package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestParseAge(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"90d", 90 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"720h", 720 * time.Hour, false},
		{"36h30m", 36*time.Hour + 30*time.Minute, false},
		{"", 0, true},
		{"0d", 0, true},
		{"-5d", 0, true},
		{"d", 0, true},
		{"ninety days", 0, true},
	}
	for _, tt := range tests {
		got, err := parseAge(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseAge(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("parseAge(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// The backup lands in the state directory, named for when it was taken, so
// flushes never overwrite each other's backups.
func TestDefaultFlushBackupPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	got, err := defaultFlushBackupPath(time.Date(2026, 9, 11, 13, 5, 9, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "memstore", "flush-20260911-130509.json")
	if got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}
