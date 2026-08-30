package webdoc_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/memstore/internal/webdoc"
)

func TestIsURL(t *testing.T) {
	cases := map[string]bool{
		"https://example.com/a": true,
		"http://example.com":    true,
		"HTTPS://example.com":   true,
		"./docs/schema.md":      false,
		"/home/m/git/memstore":  false,
		"docs/schema.md":        false,
		"file:///etc/passwd":    false,
		"ftp://example.com/x":   false,
		"C:/Users/m/notes.md":   false,
		"":                      false,
	}
	for in, want := range cases {
		if got := webdoc.IsURL(in); got != want {
			t.Errorf("IsURL(%q) = %v, want %v", in, got, want)
		}
	}
}

// The path is the document's identity: UNIQUE (namespace, user_id, repo_url,
// path) is what makes re-ingesting a page replace it rather than duplicate it.
// So it must be stable across runs, and it must end in .md or the daemon's
// routeForPath will not route it to the markdown chunker at all.
func TestDocPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://gist.github.com/karpathy/442a6bf5", "gist.github.com/karpathy/442a6bf5.md"},
		{"https://example.com", "example.com/index.md"},
		{"https://example.com/", "example.com/index.md"},
		{"https://example.com/a/b/", "example.com/a/b/index.md"},
		{"http://example.com/paper.html", "example.com/paper.html.md"},
		// A fragment names a place inside one document, not another document.
		{"https://example.com/a#results", "example.com/a.md"},
	}
	for _, c := range cases {
		got, err := webdoc.DocPath(c.in)
		if err != nil {
			t.Errorf("DocPath(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("DocPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// A query selects different content, so it must not collide with the
	// bare path -- but it cannot go in verbatim either.
	q1, _ := webdoc.DocPath("https://example.com/s?q=one")
	q2, _ := webdoc.DocPath("https://example.com/s?q=two")
	bare, _ := webdoc.DocPath("https://example.com/s")
	if q1 == q2 || q1 == bare || q2 == bare {
		t.Errorf("query paths collide: %q %q %q", q1, q2, bare)
	}
	if strings.ContainsAny(q1, "?&=") {
		t.Errorf("DocPath kept raw query punctuation: %q", q1)
	}

	// Stability: the same URL twice is the same document.
	a, _ := webdoc.DocPath("https://example.com/s?q=one")
	if a != q1 {
		t.Errorf("DocPath is not stable: %q then %q", q1, a)
	}
}

// The path reaches a UNIQUE key and a display column. Nothing derived from a
// remote URL may escape its host directory or climb out of the corpus.
func TestDocPathRefusesTraversal(t *testing.T) {
	for _, in := range []string{
		"https://example.com/../../etc/passwd",
		"https://example.com/%2e%2e%2f%2e%2e%2fetc/passwd",
		"https://example.com/a/%2F%2E%2E/b",
		"https://" + strings.Repeat("a", 300) + ".com/x",
	} {
		got, err := webdoc.DocPath(in)
		if err != nil {
			continue // refusing outright is a fine answer
		}
		if strings.Contains(got, "..") || strings.HasPrefix(got, "/") || strings.Contains(got, "//") {
			t.Errorf("DocPath(%q) = %q escapes or malforms", in, got)
		}
		if len(got) > 200 {
			t.Errorf("DocPath(%q) is %d bytes; unbounded path", in, len(got))
		}
	}
}

func TestConvert(t *testing.T) {
	md, review, err := webdoc.Convert(`<h1>Title</h1><p>Some <em>text</em>.</p><ul><li>one</li><li>two</li></ul><pre><code>x := 1</code></pre>`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Title", "*text*", "- one", "- two", "x := 1"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
	if review {
		t.Errorf("clean HTML should not need review:\n%s", md)
	}
}

// The flag must not fire on a page that merely talks about HTML. Borrowed
// from faq-import, which learned it on technical posts that quote markup
// verbatim, and still worth pinning here even though the detector underneath
// it changed.
func TestConvertDoesNotFlagQuotedHTML(t *testing.T) {
	_, review, err := webdoc.Convert(`<p>Wrap it in <code>&lt;div id="content"&gt;</code> like so.</p>`)
	if err != nil {
		t.Fatal(err)
	}
	if review {
		t.Error("HTML quoted inside code is documentation, not conversion failure")
	}

	_, review, err = webdoc.Convert("<pre><code>&lt;html&gt;&lt;body&gt;hi&lt;/body&gt;&lt;/html&gt;</code></pre>")
	if err != nil {
		t.Fatal(err)
	}
	if review {
		t.Error("HTML inside a fenced code block is content, not conversion failure")
	}
}

func newServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

var fixedNow = func() time.Time { return time.Date(2026, 8, 30, 9, 40, 0, 0, time.UTC) }

// Since the fetched bytes are discarded, the only route from a stored chunk
// back to where it came from is text inside the document itself. The
// provenance header is therefore part of the document, not decoration.
func TestFetchWritesProvenanceIntoTheDocument(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>Chunk sizes</title></head><body><h2>Findings</h2><p>Small wins.</p></body></html>`))
	})

	res, err := webdoc.Fetch(context.Background(), s.Client(), s.URL+"/paper", webdoc.Options{Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Markdown, s.URL+"/paper") {
		t.Errorf("document does not name its source URL:\n%s", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "2026-08-30T09:40:00Z") {
		t.Errorf("document does not record when it was retrieved:\n%s", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "Small wins.") {
		t.Errorf("converted body missing:\n%s", res.Markdown)
	}
	// The daemon derives a document title from the markdown, so the page
	// title has to lead it.
	if !strings.HasPrefix(res.Markdown, "# Chunk sizes\n") {
		t.Errorf("document should lead with the page title:\n%s", res.Markdown)
	}
	if res.Title != "Chunk sizes" {
		t.Errorf("Title = %q", res.Title)
	}
	if res.ContentType != "text/html" {
		t.Errorf("ContentType = %q", res.ContentType)
	}
}

// PDFs are a known gap, not a silent failure: a large share of research
// papers are PDFs and extracting them wrongly would satisfy the corpus
// invariant while breaking what it is for (docs/document-ingest.md, task
// 8415). Refuse, and say why.
func TestFetchRefusesPDF(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.7\n"))
	})
	_, err := webdoc.Fetch(context.Background(), s.Client(), s.URL+"/paper.pdf", webdoc.Options{Now: fixedNow})
	if err == nil {
		t.Fatal("PDF was accepted")
	}
	var ute *webdoc.UnsupportedTypeError
	if !errorAs(err, &ute) {
		t.Fatalf("err = %v, want UnsupportedTypeError so the caller can explain the gap", err)
	}
	if ute.ContentType != "application/pdf" {
		t.Errorf("ContentType = %q", ute.ContentType)
	}
}

func TestFetchLimitsAndFailures(t *testing.T) {
	t.Run("oversize body is refused", func(t *testing.T) {
		s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<p>" + strings.Repeat("a", 4096) + "</p>"))
		})
		if _, err := webdoc.Fetch(context.Background(), s.Client(), s.URL, webdoc.Options{Now: fixedNow, MaxBytes: 1024}); err == nil {
			t.Error("oversize body accepted; an unbounded read is a remote-controlled allocation")
		}
	})

	t.Run("non-2xx is an error", func(t *testing.T) {
		s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "gone", http.StatusNotFound)
		})
		if _, err := webdoc.Fetch(context.Background(), s.Client(), s.URL, webdoc.Options{Now: fixedNow}); err == nil {
			t.Error("404 accepted as a document")
		}
	})

	t.Run("plain text passes through unconverted", func(t *testing.T) {
		s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("# Already markdown\n\nBody.\n"))
		})
		res, err := webdoc.Fetch(context.Background(), s.Client(), s.URL+"/raw.md", webdoc.Options{Now: fixedNow})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Markdown, "# Already markdown") || strings.Contains(res.Markdown, "\\#") {
			t.Errorf("text/plain should not be run through the HTML converter:\n%s", res.Markdown)
		}
	})
}

// A table is the shape research material loses most readily. Without the
// table plugin this two-by-two converts to "ab12" -- every cell present, all
// structure gone, and nothing in the output saying so.
func TestConvertKeepsTables(t *testing.T) {
	md, _, err := webdoc.Convert(`<table><tr><th>model</th><th>score</th></tr><tr><td>small</td><td>0.91</td></tr></table>`)
	if err != nil {
		t.Fatal(err)
	}
	// Cell padding is the plugin's business; the structure is ours.
	norm := regexp.MustCompile(` +`).ReplaceAllString(md, " ")
	if !strings.Contains(norm, "| model | score |") || !strings.Contains(norm, "| small | 0.91 |") {
		t.Errorf("table did not survive conversion:\n%s", md)
	}
	if strings.Contains(md, "ab12") || !strings.Contains(md, "|") {
		t.Errorf("table flattened to a run-on string:\n%s", md)
	}
}

// The converter drops what it cannot represent and keeps the text inside, so
// the output reads as complete prose with a figure or an equation silently
// missing. With no raw copy retained, a source-side check is the only place
// that loss is visible at all.
func TestLossyElementsAreFlagged(t *testing.T) {
	cases := map[string]string{
		"equation":       `<p>Given <math><mi>x</mi></math> we derive.</p>`,
		"figure":         `<p>See <svg width="10"><title>Figure 1</title><circle r="5"/></svg> above.</p>`,
		"embedded video": `<p>intro</p><iframe src="https://example.com/v"></iframe>`,
	}
	for name, src := range cases {
		_, review, err := webdoc.Convert(src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !review {
			t.Errorf("%s: conversion lost content without flagging it", name)
		}
	}

	if got := webdoc.LossyElements(`<p>plain <em>prose</em> with a <a href="/x">link</a>.</p>`); got != nil {
		t.Errorf("ordinary prose flagged as lossy: %v", got)
	}
	got := webdoc.LossyElements(`<math><mi>x</mi></math><svg><title>Fig</title></svg><math></math>`)
	if len(got) != 2 || got[0] != "math" || got[1] != "svg" {
		t.Errorf("LossyElements = %v, want [math svg] deduplicated and sorted", got)
	}

	// A page built from web components and icon svgs is an ordinary modern
	// page, not a failed conversion. Flagging it made the warning fire on
	// every real site and therefore mean nothing.
	chrome := `<main><clipboard-copy><svg><circle/></svg></clipboard-copy><tool-tip>copy</tool-tip>` +
		`<action-menu></action-menu><p>The actual article text.</p></main>`
	if got := webdoc.LossyElements(chrome); got != nil {
		t.Errorf("site chrome flagged as content loss: %v", got)
	}
}

// A page is mostly not its content. Storing the chrome puts "Skip to
// content", "Sign in" and a nav menu into the corpus as searchable chunks,
// and it is also what made the review flag fire on a site's UI icons rather
// than on anything lost from the article.
func TestConvertUsesMainContent(t *testing.T) {
	page := `<html><head><title>Paper</title></head><body>
	  <header><nav><a href="/x">Skip to content</a><a href="/in">Sign in</a><svg><circle/></svg></nav></header>
	  <main><h2>Findings</h2><p>Small consistent chunks win.</p></main>
	  <footer><p>(c) 2026 Example</p><svg><rect/></svg></footer>
	</body></html>`

	md, review, err := webdoc.Convert(page)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "Small consistent chunks win.") {
		t.Errorf("lost the article body:\n%s", md)
	}
	for _, junk := range []string{"Skip to content", "Sign in", "(c) 2026 Example"} {
		if strings.Contains(md, junk) {
			t.Errorf("chrome %q stored as content:\n%s", junk, md)
		}
	}
	if review {
		t.Error("review flag fired on chrome icons outside the content; it must judge the content only")
	}
}

func TestConvertFallsBackWithoutMain(t *testing.T) {
	md, _, err := webdoc.Convert(`<html><body><h2>Bare</h2><p>No main element here.</p></body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "No main element here.") {
		t.Errorf("fallback lost the body:\n%s", md)
	}

	// <article> is the other common wrapper, and it should win over a bare
	// body just as <main> does.
	md, _, err = webdoc.Convert(`<html><body><nav>Menu</nav><article><p>The piece itself.</p></article></body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "The piece itself.") || strings.Contains(md, "Menu") {
		t.Errorf("article extraction wrong:\n%s", md)
	}
}
