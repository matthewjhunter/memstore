package httpapi

import (
	"testing"
)

func TestLearnSessionStore_RecordAndConsume(t *testing.T) {
	ls := NewLearnSessionStore()
	defer ls.Stop()

	refs := []learnedFactRef{
		{FactID: 1, Surface: "file", RelPath: "main.go", Subject: "proj"},
		{FactID: 2, Surface: "symbol", RelPath: "main.go", Subject: "proj"},
	}
	ls.Record("sess1", "proj", refs)

	sess := ls.Consume("sess1")
	if sess == nil {
		t.Fatal("expected session")
	}
	if len(sess.facts) != 2 {
		t.Errorf("expected 2 facts, got %d", len(sess.facts))
	}
	if sess.subject != "proj" {
		t.Errorf("expected subject 'proj', got %q", sess.subject)
	}

	// Second consume should return nil.
	if ls.Consume("sess1") != nil {
		t.Error("expected nil after consume")
	}
}

func TestLearnSessionStore_AccumulatesAcrossCalls(t *testing.T) {
	ls := NewLearnSessionStore()
	defer ls.Stop()

	ls.Record("sess1", "proj", []learnedFactRef{
		{FactID: 1, Surface: "file"},
	})
	ls.Record("sess1", "proj", []learnedFactRef{
		{FactID: 2, Surface: "doc"},
	})

	sess := ls.Consume("sess1")
	if sess == nil {
		t.Fatal("expected session")
	}
	if len(sess.facts) != 2 {
		t.Errorf("expected 2 accumulated facts, got %d", len(sess.facts))
	}
}

func TestLearnSessionStore_IsolatedSessions(t *testing.T) {
	ls := NewLearnSessionStore()
	defer ls.Stop()

	ls.Record("sess1", "proj1", []learnedFactRef{{FactID: 1}})
	ls.Record("sess2", "proj2", []learnedFactRef{{FactID: 2}, {FactID: 3}})

	s1 := ls.Consume("sess1")
	s2 := ls.Consume("sess2")

	if len(s1.facts) != 1 {
		t.Errorf("sess1: expected 1 fact, got %d", len(s1.facts))
	}
	if len(s2.facts) != 2 {
		t.Errorf("sess2: expected 2 facts, got %d", len(s2.facts))
	}
}

func TestLearnSessionStore_EmptySessionID(t *testing.T) {
	ls := NewLearnSessionStore()
	defer ls.Stop()

	ls.Record("", "proj", []learnedFactRef{{FactID: 1}})

	if ls.Consume("") != nil {
		t.Error("empty session ID should not store/return")
	}
}
