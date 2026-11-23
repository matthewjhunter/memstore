package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore"
)

func TestParseRetention(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"": 0, "0": 0, " 90d ": 90 * 24 * time.Hour, "2160h": 2160 * time.Hour, "1d": 24 * time.Hour,
	} {
		got, err := parseRetention(in)
		if err != nil || got != want {
			t.Errorf("parseRetention(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"ninety", "-5d", "-1h", "d", "1.5d"} {
		if _, err := parseRetention(in); err == nil {
			t.Errorf("parseRetention(%q) accepted", in)
		}
	}
}

type fakeIndex struct {
	expired []memstore.ExpiredArchive
	marked  []int64
	markErr error
}

func (f *fakeIndex) ExpiredArchives(context.Context, time.Time) ([]memstore.ExpiredArchive, error) {
	return f.expired, nil
}

func (f *fakeIndex) MarkArchiveRemoved(_ context.Context, id int64) error {
	if f.markErr != nil {
		return f.markErr
	}
	f.marked = append(f.marked, id)
	return nil
}

type fakeFiles struct {
	removed []string
	failOn  string
}

func (f *fakeFiles) Remove(_ int64, sessionID string) error {
	if sessionID == f.failOn {
		return errors.New("disk says no")
	}
	f.removed = append(f.removed, sessionID)
	return nil
}

func TestSweepRemovesTheFileThenMarksTheRow(t *testing.T) {
	idx := &fakeIndex{expired: []memstore.ExpiredArchive{{ID: 1, UserID: 7, SessionID: "a"}, {ID: 2, UserID: 7, SessionID: "b"}}}
	files := &fakeFiles{}
	n, err := sweepTranscripts(context.Background(), idx, files, time.Now())
	if err != nil || n != 2 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	if len(files.removed) != 2 || len(idx.marked) != 2 {
		t.Errorf("removed %v, marked %v", files.removed, idx.marked)
	}
}

// A file that could not be removed is not recorded as removed.
func TestSweepDoesNotMarkWhatItCouldNotRemove(t *testing.T) {
	idx := &fakeIndex{expired: []memstore.ExpiredArchive{{ID: 1, UserID: 7, SessionID: "a"}, {ID: 2, UserID: 7, SessionID: "b"}}}
	files := &fakeFiles{failOn: "b"}
	n, err := sweepTranscripts(context.Background(), idx, files, time.Now())
	if err == nil || n != 1 {
		t.Fatalf("sweep = %d, %v; want 1 and an error", n, err)
	}
	if len(idx.marked) != 1 || idx.marked[0] != 1 {
		t.Errorf("marked = %v, want only the first", idx.marked)
	}
}
