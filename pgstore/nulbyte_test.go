package pgstore

import (
	"encoding/json"
	"strings"
	"testing"
)

// nulEscape is the six-character JSON escape for a NUL, assembled rather than
// written literally so this file contains no control character of its own.
var nulEscape = `\` + `u0000`

// A NUL reaches the store the way it reached it in production: a Claude Code
// transcript is JSONL, a turn that captured terminal output carries the escape
// above, and decoding it yields a real NUL in the Go string. Postgres then
// rejects the insert with SQLSTATE 22021, and that transcript can never be
// uploaded -- not on this attempt, not on any retry.
func TestStripNUL_DecodedFromATranscriptEscape(t *testing.T) {
	var turn struct {
		Content string `json:"content"`
	}
	raw := `{"content":"before` + nulEscape + `after"}`
	if err := json.Unmarshal([]byte(raw), &turn); err != nil {
		t.Fatal(err)
	}
	if !strings.ContainsRune(turn.Content, 0) {
		t.Fatal("fixture did not decode to a NUL; the test is not testing anything")
	}

	got := stripNUL(turn.Content)
	if strings.ContainsRune(got, 0) {
		t.Error("a NUL survived stripNUL")
	}
	if got != "beforeafter" {
		t.Errorf("stripNUL = %q, want %q", got, "beforeafter")
	}
}

// Every turn of every upload goes through this, so clean text must pass without
// allocating a second copy.
func TestStripNUL_LeavesCleanTextAlone(t *testing.T) {
	const clean = "ordinary content, café included"
	if got := stripNUL(clean); got != clean {
		t.Errorf("stripNUL rewrote clean text: %q", got)
	}
	if n := testing.AllocsPerRun(100, func() { stripNUL(clean) }); n != 0 {
		t.Errorf("stripNUL allocated %v times on clean text", n)
	}
}

// JSONB refuses the escape even though it is valid JSON, so a hook payload has
// to be cleaned in its encoded form -- and must still parse afterwards.
func TestStripNULJSON_RemovesBothForms(t *testing.T) {
	payload := []byte(`{"session_id":"s1","cwd":"/tmp` + nulEscape + `x"}`)
	cleaned := stripNULJSON(payload)

	if strings.Contains(string(cleaned), nulEscape) {
		t.Error("the escape survived stripNULJSON")
	}
	var got map[string]any
	if err := json.Unmarshal(cleaned, &got); err != nil {
		t.Fatalf("cleaned payload is no longer valid JSON: %v", err)
	}
	if got["cwd"] != "/tmpx" || got["session_id"] != "s1" {
		t.Errorf("payload damaged beyond the NUL: %v", got)
	}

	// A raw NUL byte is invalid JSON to begin with, but it must not reach the
	// driver either.
	if strings.ContainsRune(string(stripNULJSON([]byte("{\"a\":\"b\x00c\"}"))), 0) {
		t.Error("a raw NUL byte survived stripNULJSON")
	}
}

func TestStripNULJSON_LeavesCleanPayloadAlone(t *testing.T) {
	clean := []byte(`{"session_id":"s1","cwd":"/home/m"}`)
	if got := stripNULJSON(clean); string(got) != string(clean) {
		t.Errorf("stripNULJSON rewrote a clean payload: %s", got)
	}
}
