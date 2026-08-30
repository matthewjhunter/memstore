package main

// memstore ingest <url>: the remote half of docs/document-ingest.md.
//
// The fetch happens here rather than in the daemon on purpose. A daemon that
// retrieves arbitrary URLs on request is an SSRF proxy for anyone holding an
// ingest token, and it would invert the division this command already
// follows, where the client produces bytes and the daemon verifies and chunks
// them.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/matthewjhunter/memstore/httpclient"
	"github.com/matthewjhunter/memstore/internal/webdoc"
)

// fetchTimeout bounds one retrieval end to end.
const fetchTimeout = 60 * time.Second

// ingestURL fetches, converts, and ships one remote document. Returns the
// number of failures, matching ingestFile.
func ingestURL(ctx context.Context, client *httpclient.Client, rawURL string) int {
	hc := &http.Client{Timeout: fetchTimeout}
	res, err := webdoc.Fetch(ctx, hc, rawURL, webdoc.Options{})
	if err != nil {
		var ute *webdoc.UnsupportedTypeError
		if errors.As(err, &ute) {
			fmt.Fprintf(os.Stderr, "ingest %s: %s\n", rawURL, explainUnsupported(ute.ContentType))
			return 1
		}
		fmt.Fprintf(os.Stderr, "ingest %s: %v\n", rawURL, err)
		return 1
	}

	path, err := webdoc.DocPath(rawURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ingest %s: %v\n", rawURL, err)
		return 1
	}

	content := []byte(res.Markdown)
	sum := sha256.Sum256(content)
	mtime := res.Retrieved
	// Stored as a loose document: there is no repo, and the empty RepoURL is
	// what makes the daemon record it as one. Trusted stays false server-side
	// for everything today, which is also the right answer for remote
	// material regardless of who typed the command.
	up, err := client.UploadDocument(ctx, httpclient.DocUpload{
		Path:    path,
		Content: content,
		SHA256:  hex.EncodeToString(sum[:]),
		Mtime:   &mtime,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ingest %s: %v\n", rawURL, err)
		return 1
	}

	fmt.Printf("ingested %s\n", rawURL)
	fmt.Printf("  stored as %s (%d chunks, %s, untrusted)\n", path, up.Chunks, up.Strategy)
	if res.Title != "" {
		fmt.Printf("  title: %s\n", res.Title)
	}
	reportLoss(res)
	return 0
}

// reportLoss says what the conversion could not carry. We keep no copy of the
// fetched bytes, so this warning is the only notice that anything went
// missing -- printing it is not a nicety, it is the whole of the audit trail.
func reportLoss(res webdoc.Result) {
	if !res.NeedsReview {
		return
	}
	fmt.Printf("  NEEDS REVIEW: the conversion may have dropped content.\n")
	fmt.Printf("  The fetched bytes are not kept, so check the page against what was stored.\n")
}

// explainUnsupported turns a refused content type into the reason, naming the
// PDF gap explicitly because that is the one a user will hit while trying to
// ingest exactly the material this feature exists for.
func explainUnsupported(ct string) string {
	if strings.Contains(ct, "pdf") {
		return "PDFs are not supported yet.\n" +
			"  This is a known gap, not a bug: extracting a PDF wrongly would satisfy the corpus\n" +
			"  invariant while breaking what it is for (docs/document-ingest.md). Save the paper\n" +
			"  locally and ingest it as a file once PDF support lands."
	}
	return fmt.Sprintf("cannot convert content type %q; only HTML, plain text and markdown are supported", ct)
}
