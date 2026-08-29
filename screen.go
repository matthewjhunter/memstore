package memstore

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/matthewjhunter/airlock/detect"
)

// Screening vocabulary shared by every backend. The SQLite implementation
// these lived beside was removed in 0.6.0; PostgreSQL is the only store.

// ScreenableText composes what the screener actually judges: the fact's content plus
// every metadata string value.
//
// Metadata has to be in here. It is rendered to models alongside content, it is
// writable through a second path (UpdateMetadata), and it is where an attacker would
// go the moment content alone is screened.
//
// Every string value, with no length floor. An earlier version skipped values under 80
// runes on the theory that short values are enum-ish and cannot say much -- which is
// false, and the tests said so immediately: "ignore all previous instructions" is 32
// characters and a complete attack, so the floor was a documented hole exactly the
// size of the most common payload. The renderer has a length threshold because
// inline-versus-fenced is a layout question; screening has no equivalent excuse, and
// scanning a few extra short strings costs nothing.
//
// Values are walked recursively, so a payload nested inside an object or array is
// found rather than skipped for not being a top-level string.
func ScreenableText(content, metadata string) string {
	if metadata == "" || metadata == "null" {
		return content
	}
	var parsed any
	if err := json.Unmarshal([]byte(metadata), &parsed); err != nil {
		// Unparseable metadata gets screened whole: its shape is unknown, so no part
		// of it can be dismissed as too short to matter.
		return content + "\n\n" + metadata
	}

	var values []string
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			if strings.TrimSpace(t) != "" {
				values = append(values, t)
			}
		case map[string]any:
			for _, vv := range t {
				walk(vv)
			}
		case []any:
			for _, vv := range t {
				walk(vv)
			}
		}
	}
	walk(parsed)

	if len(values) == 0 {
		return content
	}
	return content + "\n\n" + strings.Join(values, "\n\n")
}

// BlockedFact is a rejected write, surfaced for review.
//
// Content is included: an operator deciding whether a block was a false positive has
// to see what was blocked, and this content exists nowhere else -- a blocked write
// never becomes a readable fact. Callers rendering it to a model must fence it, the
// same as any stored content.
type BlockedFact struct {
	ID        int64
	Subject   string
	Content   string
	Threat    int
	Category  string
	Reason    string
	CreatedAt time.Time
}

// InlineRejectScore is the detect aggregate at which an inline screen rejects a write.
// 80 is a single high-severity rule in airlock's corpus; corroboration across
// categories scores higher.
const InlineRejectScore = 80

// ErrScreenRejected reports a write refused by injection screening. Callers should
// surface it to the user rather than retrying: the content will be refused again.
var ErrScreenRejected = errors.New("memstore: write rejected by injection screening")

func DetectRuleIDs(r detect.Result) []string {
	out := make([]string, 0, len(r.Matches))
	for _, m := range r.Matches {
		out = append(out, m.Rule)
	}
	return out
}
