package httpapi

import (
	"testing"
)

func TestSessionContext_TouchAndRetrieve(t *testing.T) {
	sc := NewSessionContext()
	defer sc.Stop()

	sc.TouchFiles("s1", []string{"/a/foo.go", "/a/bar.go"})
	sc.TouchFiles("s1", []string{"/a/baz.go"})

	files := sc.RecentFiles("s1")
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(files))
	}
	if files[0] != "/a/foo.go" || files[1] != "/a/bar.go" || files[2] != "/a/baz.go" {
		t.Fatalf("unexpected files: %v", files)
	}
}

func TestSessionContext_Deduplication(t *testing.T) {
	sc := NewSessionContext()
	defer sc.Stop()

	sc.TouchFiles("s1", []string{"/a/foo.go", "/a/bar.go"})
	sc.TouchFiles("s1", []string{"/a/foo.go"}) // duplicate, should move to end

	files := sc.RecentFiles("s1")
	if len(files) != 2 {
		t.Fatalf("expected 2 files after dedup, got %d", len(files))
	}
	if files[0] != "/a/bar.go" || files[1] != "/a/foo.go" {
		t.Fatalf("expected foo.go moved to end: %v", files)
	}
}

func TestSessionContext_RingBuffer(t *testing.T) {
	sc := NewSessionContext()
	defer sc.Stop()

	// Add more than maxRecentFiles.
	for i := 0; i < maxRecentFiles+5; i++ {
		sc.TouchFiles("s1", []string{"/a/" + string(rune('A'+i)) + ".go"})
	}

	files := sc.RecentFiles("s1")
	if len(files) != maxRecentFiles {
		t.Fatalf("expected %d files, got %d", maxRecentFiles, len(files))
	}
}

func TestSessionContext_EmptySession(t *testing.T) {
	sc := NewSessionContext()
	defer sc.Stop()

	files := sc.RecentFiles("nonexistent")
	if files != nil {
		t.Fatalf("expected nil for unknown session, got %v", files)
	}
}

func TestSessionContext_EmptySessionID(t *testing.T) {
	sc := NewSessionContext()
	defer sc.Stop()

	sc.TouchFiles("", []string{"/a/foo.go"})
	files := sc.RecentFiles("")
	if files != nil {
		t.Fatalf("expected nil for empty session ID, got %v", files)
	}
}

func TestSessionContext_IsolatedSessions(t *testing.T) {
	sc := NewSessionContext()
	defer sc.Stop()

	sc.TouchFiles("s1", []string{"/a/foo.go"})
	sc.TouchFiles("s2", []string{"/b/bar.go"})

	f1 := sc.RecentFiles("s1")
	f2 := sc.RecentFiles("s2")
	if len(f1) != 1 || f1[0] != "/a/foo.go" {
		t.Fatalf("s1 files wrong: %v", f1)
	}
	if len(f2) != 1 || f2[0] != "/b/bar.go" {
		t.Fatalf("s2 files wrong: %v", f2)
	}
}
