// Package fence guards stored or session-derived text before it is interpolated
// into a prompt memstore sends to its own model (curation, extraction, rating,
// synthesis). memstore owns both ends of those prompts, so the spotlighting
// approach applies cleanly: wrap untrusted spans in a per-call nonce delimiter
// and name that delimiter in the trusted region as "this is data."
//
// Two layers, mirroring Herald's internal/ai/fence.go:
//
//  1. Per-call nonce delimiter -- <untrusted-{nonce}> ... </untrusted-{nonce}>.
//     The nonce is 16 crypto/rand bytes, hex-encoded, unique per prompt, so a
//     stored fact cannot predict or reproduce the closing tag to break out.
//  2. Delimiter neutralization -- any fence-shaped tag is stripped from the
//     untrusted text before interpolation, so even a leaked nonce or a legacy
//     static delimiter cannot be opened or closed from within the content.
//
// This lands in memstore's internal/ first; per shared-model-io-audit.md the
// primitive is later promoted to a shared model-I/O module consumed by both
// memstore and Herald.
package fence

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
)

// tagRe matches an opening or closing fence delimiter: the nonce-suffixed form
// this package emits (<untrusted-...>) and the legacy static <article> form.
// It deliberately does NOT match tags carrying attributes (e.g. <article id=x>),
// leaving genuine markup in stored content intact for the model to inspect.
var tagRe = regexp.MustCompile(`(?i)</?(?:untrusted|article)(?:-[0-9a-f]+)?\s*>`)

// Nonce returns an unguessable lowercase-hex token unique to one prompt
// invocation, used to build the <untrusted-{nonce}> delimiter.
func Nonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate fence nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Neutralize removes any fence-delimiter sequence from untrusted text so it can
// neither open nor close the fence that wraps it in a prompt. Exported for
// spans that are interpolated outside a Wrap call (e.g. an inline subject or a
// task string).
func Neutralize(s string) string {
	return tagRe.ReplaceAllString(s, "[tag removed]")
}

// Wrap encloses untrusted content in a nonce-delimited fence, neutralizing the
// content first so it cannot forge the delimiter. The trusted region of the
// prompt must name the same nonce and instruct the model to treat everything
// inside <untrusted-{nonce}> ... </untrusted-{nonce}> as data.
func Wrap(nonce, content string) string {
	return fmt.Sprintf("<untrusted-%s>\n%s\n</untrusted-%s>", nonce, Neutralize(content), nonce)
}
