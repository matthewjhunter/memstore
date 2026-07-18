package fence

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// InlineValueMaxRunes is the longest metadata value rendered inline, outside the
// fence. Longer values are fenced instead.
//
// The number is a shape heuristic, not a security boundary -- the fence is the
// boundary. It only decides which side of it a value lands on. 80 runes is about as
// much as a genuine structured value needs ("2026-05-01", "in_progress", a project
// slug, a model name) and well short of the room an instruction wants.
const InlineValueMaxRunes = 80

// InlineKeyMaxRunes bounds how much of a key name is shown inline. Keys are validated
// elsewhere to alphanumerics and underscores, so they cannot contain a newline or a
// fence tag, but nothing bounds their length.
const InlineKeyMaxRunes = 40

// Metadata renders a fact's metadata JSON for a model, splitting it by value shape.
//
// # Why this is not a key whitelist
//
// Metadata is deliberately open: the point is to carry whatever a domain needs, and
// field names are expected to grow. So nothing here looks at what a key is called.
// A rule keyed on recognized names would either reject useful data or quietly grant
// trusted rendering to any key someone happens to add later -- the security property
// would drift every time the schema did.
//
// Instead the split is on the value itself. A short, single-line scalar has no room
// to carry an instruction and stays inline where it is readable. Everything else --
// long strings, anything with a newline or control character, objects, arrays, and
// any blob that does not parse as JSON at all -- goes inside the fence. Unknown shape
// is treated as untrusted, which is the direction an unknown should fail in.
//
// The caller supplies the label (e.g. "metadata" or "link metadata") and the indent
// for the fenced block.
func (f Fence) Metadata(raw json.RawMessage, label, indent string) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		// Not an object, or not JSON. Nothing can be said about its shape, so all of
		// it goes inside the fence.
		return fmt.Sprintf("%s%s:\n%s\n", indent, label, f.Indent(string(raw), indent))
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var inline []string
	var fenced []string
	for _, k := range keys {
		disp := truncateRunes(f.Inline(k), InlineKeyMaxRunes)
		if s, ok := inlineScalar(m[k]); ok {
			inline = append(inline, disp+"="+f.Inline(s))
			continue
		}
		fenced = append(fenced, disp+": "+string(m[k]))
	}

	var b strings.Builder
	if len(inline) > 0 {
		fmt.Fprintf(&b, "%s%s: %s\n", indent, label, strings.Join(inline, " "))
	}
	if len(fenced) > 0 {
		fmt.Fprintf(&b, "%s%s (unstructured):\n%s\n",
			indent, label, f.Indent(strings.Join(fenced, "\n"), indent))
	}
	return b.String()
}

// inlineScalar reports whether v is a scalar small and plain enough to render outside
// the fence, and returns its display form.
//
// Objects and arrays always fail: their contents are unbounded and nesting would need
// this judgment applied recursively, which is more machinery than the readability of
// an inline value is worth.
func inlineScalar(v json.RawMessage) (string, bool) {
	var any any
	if err := json.Unmarshal(v, &any); err != nil {
		return "", false
	}
	switch t := any.(type) {
	case nil:
		return "null", true
	case bool:
		return strconv.FormatBool(t), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case string:
		if len([]rune(t)) > InlineValueMaxRunes || !isPlainLine(t) {
			return "", false
		}
		return t, true
	default: // map, slice
		return "", false
	}
}

// isPlainLine reports whether s is a single line free of control characters. A value
// carrying a newline can forge the line-oriented structure of the surrounding output
// even when it is too short to say much.
func isPlainLine(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\r' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
