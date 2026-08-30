// Package webdoc fetches a URL and turns it into the markdown document that
// memstore stores.
//
// The rule it implements is settled in docs/document-ingest.md: the converted
// markdown *is* the document, hashed and chunked like any file, and the
// fetched bytes are not kept. That decision is what shapes this package. With
// no original to fall back on, two things have to be true of the output. It
// has to carry its own provenance, because a chunk's only route back to where
// it came from is text inside the document itself. And a conversion that went
// wrong has to be visible rather than silent, which is what NeedsReview is
// for -- borrowed from faq-import's BodyConverter, which learned it on a
// StackExchange dump.
//
// Fetching lives on the client side, in the CLI, and deliberately not in the
// daemon. A daemon that retrieves arbitrary URLs on request is an SSRF proxy
// for anyone holding an ingest token, and it would invert this project's
// existing division of labour, where the client ships bytes and the daemon
// verifies and chunks them.
package webdoc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	htmlmd "github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"golang.org/x/net/html"
)

// DefaultMaxBytes bounds a response body. A remote server decides how much it
// sends, so an unbounded read is an allocation someone else controls.
const DefaultMaxBytes int64 = 8 << 20

// maxPathBytes caps a derived document path. The path reaches a UNIQUE key
// and a display column, and a hostile URL is arbitrarily long.
const maxPathBytes = 200

// UnsupportedTypeError says a URL resolved to something this package
// deliberately will not convert. It is a distinct type so the CLI can explain
// the gap -- PDFs especially, which are wanted and unsolved (task 8415).
type UnsupportedTypeError struct {
	URL         string
	ContentType string
}

func (e *UnsupportedTypeError) Error() string {
	return fmt.Sprintf("%s: cannot ingest content type %q", e.URL, e.ContentType)
}

// Result is one fetched and converted document.
type Result struct {
	URL         string
	Title       string
	ContentType string // media type only, parameters stripped
	Retrieved   time.Time
	Markdown    string // the document: title, provenance, converted body
	NeedsReview bool   // raw HTML survived conversion; see package doc
}

// Options tunes a fetch. The zero value is usable.
type Options struct {
	Now       func() time.Time
	MaxBytes  int64
	UserAgent string
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now().UTC()
}

func (o Options) maxBytes() int64 {
	if o.MaxBytes > 0 {
		return o.MaxBytes
	}
	return DefaultMaxBytes
}

func (o Options) userAgent() string {
	if o.UserAgent != "" {
		return o.UserAgent
	}
	return "memstore-ingest"
}

// IsURL reports whether s names a remote document rather than a path on disk.
// Only http and https qualify: file:// is a local path wearing a URL, and
// admitting it would route a local file down the untrusted remote branch.
func IsURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return u.Host != ""
	}
	return false
}

// Fetch retrieves u through hc and converts it.
func Fetch(ctx context.Context, hc *http.Client, u string, opts Options) (Result, error) {
	if !IsURL(u) {
		return Result{}, fmt.Errorf("%s: not an http(s) URL", u)
	}
	if hc == nil {
		hc = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("User-Agent", opts.userAgent())
	req.Header.Set("Accept", "text/html, text/plain, text/markdown;q=0.9, */*;q=0.1")

	resp, err := hc.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Result{}, fmt.Errorf("%s: HTTP %s", u, resp.Status)
	}

	ct, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || ct == "" {
		ct = "application/octet-stream"
	}
	ct = strings.ToLower(ct)

	// Read one byte past the cap so a body sitting exactly on it is not
	// mistaken for an overrun, and an overrun is detectable at all.
	limit := opts.maxBytes()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", u, err)
	}
	if int64(len(body)) > limit {
		return Result{}, fmt.Errorf("%s: response exceeds %d bytes", u, limit)
	}

	res := Result{URL: u, ContentType: ct, Retrieved: opts.now().UTC()}

	var bodyMD string
	switch ct {
	case "text/html", "application/xhtml+xml":
		md, review, cerr := Convert(string(body))
		if cerr != nil {
			return Result{}, fmt.Errorf("%s: %w", u, cerr)
		}
		bodyMD, res.NeedsReview = md, review
		res.Title = htmlTitle(string(body))
	case "text/plain", "text/markdown", "text/x-markdown":
		bodyMD = string(body)
	default:
		return Result{}, &UnsupportedTypeError{URL: u, ContentType: ct}
	}

	title, rest := splitLeadingH1(bodyMD)
	if title != "" {
		res.Title = title
	}
	if res.Title == "" {
		res.Title = fallbackTitle(u)
	}
	res.Markdown = assemble(res.Title, u, res.Retrieved, rest)
	return res, nil
}

// assemble writes the stored document: a heading the daemon can derive a
// title from, the provenance block, then the body.
func assemble(title, srcURL string, retrieved time.Time, body string) string {
	var b strings.Builder
	b.WriteString("# ")
	b.WriteString(title)
	b.WriteString("\n\n> Source: ")
	b.WriteString(srcURL)
	b.WriteString("\n> Retrieved: ")
	b.WriteString(retrieved.UTC().Format(time.RFC3339))
	b.WriteString("\n\n")
	b.WriteString(strings.TrimLeft(body, "\n"))
	if !strings.HasSuffix(b.String(), "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// splitLeadingH1 lifts a body's own top-level heading out to be the document
// title, so the assembled document does not carry two competing ones.
func splitLeadingH1(md string) (title, rest string) {
	trimmed := strings.TrimLeft(md, "\n")
	if !strings.HasPrefix(trimmed, "# ") {
		return "", md
	}
	line, remainder, _ := strings.Cut(trimmed, "\n")
	return strings.TrimSpace(strings.TrimPrefix(line, "# ")), remainder
}

// fallbackTitle names a document whose source offered no title.
func fallbackTitle(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return u
	}
	if p := strings.Trim(parsed.Path, "/"); p != "" {
		return parsed.Host + "/" + p
	}
	return parsed.Host
}

// htmlTitle returns the document's <title>, or "".
func htmlTitle(src string) string {
	node, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return ""
	}
	var find func(*html.Node) string
	find = func(n *html.Node) string {
		if n.Type == html.ElementNode && n.Data == "title" {
			var sb strings.Builder
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.TextNode {
					sb.WriteString(c.Data)
				}
			}
			return strings.TrimSpace(sb.String())
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if t := find(c); t != "" {
				return t
			}
		}
		return ""
	}
	return find(node)
}

// Convert turns HTML into markdown, reporting whether the conversion may
// have lost something worth a human look.
//
// The plugin set is base + commonmark + table + strikethrough. The table
// plugin is not optional here and its absence is not a style choice: without
// it a results table converts to a run-on string of its cells ("ab12" for a
// two-by-two), which is silent structural loss in exactly the material this
// package exists to ingest.
func Convert(src string) (md string, needsReview bool, err error) {
	conv := htmlmd.NewConverter(
		htmlmd.WithPlugins(
			base.NewBasePlugin(),
			commonmark.NewCommonmarkPlugin(),
			table.NewTablePlugin(),
			strikethrough.NewStrikethroughPlugin(),
		),
	)
	out, err := conv.ConvertString(src)
	if err != nil {
		return "", false, fmt.Errorf("html to markdown: %w", err)
	}
	return out, LossyElements(src) != nil || DetectRawHTML(out), nil
}

// lossyElements are element types markdown has no representation for. The
// converter does not fail on them; it drops them and keeps their text, so a
// figure, an equation or an embedded viewer leaves output that reads as
// complete prose while the thing itself is gone.
var lossyElements = map[string]bool{
	"math": true, "svg": true, "canvas": true, "iframe": true,
	"object": true, "embed": true, "video": true, "audio": true,
	"frame": true, "frameset": true, "map": true,
}

// LossyElements returns the sorted element names in src that cannot survive
// conversion, or nil. Custom elements (any tag name containing a hyphen, per
// the HTML spec) count: the converter has no rule for them and keeps only
// their text.
//
// This is a source-side check, and it has to be. The output-side test
// borrowed from faq-import catches markup the converter left behind, which
// is the failure mode of the HTML that project handles. This library's
// failure mode is the opposite one -- it strips what it does not understand
// -- and nothing in the output records that anything was removed.
func LossyElements(src string) []string {
	node, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			name := strings.ToLower(n.Data)
			if lossyElements[name] || strings.Contains(name, "-") {
				seen[name] = true
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(node)
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var (
	fencedBlock = regexp.MustCompile("(?s)```.*?```|~~~.*?~~~")
	inlineCode  = regexp.MustCompile("`[^`\n]*`")
	anyTag      = regexp.MustCompile(`(?i)<\s*/?\s*([a-z][a-z0-9-]*)\b[^>]*>`)
)

// inlineAllowed are tags the commonmark plugin legitimately leaves in place;
// their presence is not evidence of a failed conversion.
var inlineAllowed = map[string]bool{
	"br": true, "sub": true, "sup": true, "kbd": true, "mark": true,
}

// DetectRawHTML reports whether md contains an HTML tag outside the inline
// allowlist, which means the converter met markup it had no rule for.
//
// Code is stripped first, and that is the whole subtlety: a page explaining
// how to write `<div id="content">` is documentation, not a broken
// conversion. faq-import found this the expensive way, on technical posts
// that quote markup verbatim.
func DetectRawHTML(md string) bool {
	stripped := inlineCode.ReplaceAllString(fencedBlock.ReplaceAllString(md, ""), "")
	for _, m := range anyTag.FindAllStringSubmatch(stripped, -1) {
		if !inlineAllowed[strings.ToLower(m[1])] {
			return true
		}
	}
	return false
}

var unsafePathChar = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// DocPath derives the loose-document path a URL is stored under.
//
// Two constraints it has to meet. The path is the document's identity --
// UNIQUE (namespace, user_id, repo_url, path) is what makes re-ingesting a
// page replace it instead of duplicating it -- so it must be stable across
// runs. And it must end in .md, or the daemon's routeForPath will not send it
// to the markdown chunker.
//
// It is also derived from remote input and lands in a display column, so
// every segment is sanitized and dot segments are dropped outright rather
// than resolved.
func DocPath(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", fmt.Errorf("%s: no host", raw)
	}

	host := sanitizeSegment(strings.ToLower(u.Hostname()))
	if host == "" {
		return "", fmt.Errorf("%s: unusable host", raw)
	}

	// u.Path is already percent-decoded, so an encoded ../ arrives here as a
	// real dot segment and is dropped with the rest.
	segs := []string{}
	for _, s := range strings.Split(u.Path, "/") {
		if strings.Trim(s, ".") == "" {
			continue // "", ".", "..", "..."
		}
		if clean := sanitizeSegment(s); clean != "" {
			segs = append(segs, clean)
		}
	}
	if len(segs) == 0 || strings.HasSuffix(u.Path, "/") {
		segs = append(segs, "index")
	}

	// A query selects different content, so it cannot collide with the bare
	// path -- but it cannot be spelled out either. A digest of it is stable,
	// bounded, and safe in a path.
	if u.RawQuery != "" {
		sum := sha256.Sum256([]byte(u.RawQuery))
		last := len(segs) - 1
		segs[last] += "-" + hex.EncodeToString(sum[:])[:8]
	}

	out := host + "/" + strings.Join(segs, "/")
	if len(out)+3 > maxPathBytes {
		sum := sha256.Sum256([]byte(out))
		keep := maxPathBytes - 3 - 9
		out = out[:keep] + "-" + hex.EncodeToString(sum[:])[:8]
	}
	return out + ".md", nil
}

// sanitizeSegment reduces one path or host segment to characters that are
// safe in a stored path, collapsing every run of anything else.
func sanitizeSegment(s string) string {
	s = unsafePathChar.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if strings.Trim(s, ".") == "" {
		return ""
	}
	return s
}
