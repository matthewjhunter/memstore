package memstore_test

import (
	"strings"
	"testing"

	embedding "github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
)

// nomic-embed-text is trained to require task prefixes: "search_document:" on
// stored text and "search_query:" on the query. Sending neither -- which
// memstore did -- means every vector comparison crosses a boundary the model
// was trained to distinguish. Nothing errors; recall is just worse.

func TestFactEmbedText_CarriesTheDocumentPrefix(t *testing.T) {
	got := memstore.FactEmbedText(chunkModel, "matthew", "prefers ASCII punctuation")

	want := embedding.FormatForTask(chunkModel, embedding.TaskRetrievalDocument, "x")
	if !strings.HasPrefix(want, "search_document:") {
		t.Fatalf("test assumes nomic document prefixes; FormatForTask gave %q", want)
	}
	if !strings.HasPrefix(got, "search_document:") {
		t.Errorf("stored text has no document prefix: %q", got)
	}
	if !strings.Contains(got, "matthew") || !strings.Contains(got, "ASCII") {
		t.Errorf("prefixing dropped the subject or the body: %q", got)
	}
}

func TestFactQueryText_CarriesTheQueryPrefix(t *testing.T) {
	got := memstore.FactQueryText(chunkModel, "when did we switch")

	if !strings.HasPrefix(got, "search_query:") {
		t.Errorf("query has no query prefix: %q", got)
	}
	if !strings.Contains(got, "when did we switch") {
		t.Errorf("prefixing dropped the query: %q", got)
	}
}

// The two sides must differ. Query-to-fact search is asymmetric retrieval; if
// both sides got the same prefix the model could not tell a question from an
// answer, which is the distinction the prefixes exist to make.
func TestFactPrefixes_QueryAndDocumentDiffer(t *testing.T) {
	const text = "the retry budget needs tuning"

	doc := memstore.FactEmbedText(chunkModel, "", text)
	query := memstore.FactQueryText(chunkModel, text)

	if doc == query {
		t.Errorf("identical text embeds identically as document and query (%q); "+
			"asymmetric retrieval needs them distinguished", doc)
	}
}

// An unregistered model must pass through unchanged rather than panicking or
// inventing a prefix. StrictModel is what makes that case loud at startup.
func TestFactPrefixes_UnknownModelPassesThrough(t *testing.T) {
	const unknown = "not-a-real-embedding-model"

	if got := memstore.FactQueryText(unknown, "hello"); got != "hello" {
		t.Errorf("query = %q, want it unchanged on an unregistered model", got)
	}
	// The record layout still applies; only the task prefix is absent.
	if got := memstore.FactEmbedText(unknown, "s", "body"); got != "subject: s\n\nbody" {
		t.Errorf("document = %q, want the unprefixed record rendering on an unregistered model", got)
	}
}

// The prefixes are set in different files for the two sides and nothing fails
// when they disagree, so the wiring is asserted end to end rather than only on
// the helpers.

func TestSearch_EmbedsTheQueryUnderTheQueryTask(t *testing.T) {
	e := &recordingEmbedder{}
	store := openTestStoreWith(t, e)
	ctx := t.Context()

	if _, err := store.Insert(ctx, memstore.Fact{
		Content: "the retry budget needs tuning", Subject: "deploy", Category: "test",
	}); err != nil {
		t.Fatal(err)
	}
	e.seen = nil

	if _, err := store.Search(ctx, "how is the retry budget set", memstore.SearchOpts{MaxResults: 5}); err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(e.seen) == 0 {
		t.Fatal("Search embedded nothing")
	}
	for i, text := range e.seen {
		if !strings.HasPrefix(text, "search_query:") {
			t.Errorf("search input %d was embedded without the query prefix: %q", i, text)
		}
	}
}

func TestEmbedFacts_EmbedsFactsUnderTheDocumentTask(t *testing.T) {
	e := &recordingEmbedder{}
	store := openTestStoreWith(t, e)
	ctx := t.Context()

	if _, err := store.Insert(ctx, memstore.Fact{
		Content: "the retry budget needs tuning", Subject: "deploy", Category: "test",
	}); err != nil {
		t.Fatal(err)
	}
	e.seen = nil

	if _, err := store.EmbedFacts(ctx, 10); err != nil {
		t.Fatalf("EmbedFacts: %v", err)
	}

	if len(e.seen) == 0 {
		t.Fatal("EmbedFacts embedded nothing")
	}
	for i, text := range e.seen {
		if !strings.HasPrefix(text, "search_document:") {
			t.Errorf("stored input %d was embedded without the document prefix: %q", i, text)
		}
		if !strings.Contains(text, "deploy") {
			t.Errorf("stored input %d lost its subject: %q", i, text)
		}
	}
}
