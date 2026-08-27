package httpapi

import (
	"database/sql"
	"net/http/httptest"
	"testing"

	"github.com/matthewjhunter/memstore"
	_ "modernc.org/sqlite"
)

// newBenchHandler builds a handler over an in-memory SQLite store. The store
// only has to be a StoreScoper here -- nothing in these tests reads a fact.
func newBenchHandler(tb testing.TB) *Handler {
	tb.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { db.Close() })
	store, err := memstore.NewSQLiteStore(db, nil, "test")
	if err != nil {
		tb.Fatal(err)
	}
	return New(store, nil, "")
}

// The cache is only a cache if it outlives the request. Constructing one per
// request would compile, pass every behavioural test, and silently restore the
// full per-request reflection cost -- so what is asserted is that the handler's
// cache is one instance, and that the servers it builds are not.
func TestSchemaCacheIsSharedAcrossRequests(t *testing.T) {
	h := newBenchHandler(t)
	if h.schemas == nil {
		t.Fatal("handler has no schema cache")
	}
	before := h.schemas

	r := httptest.NewRequest("POST", "/mcp", nil)
	first, err := h.mcpServerFor(r)
	if err != nil {
		t.Fatalf("build first server: %v", err)
	}
	second, err := h.mcpServerFor(r)
	if err != nil {
		t.Fatalf("build second server: %v", err)
	}

	if h.schemas != before {
		t.Error("the schema cache was replaced while serving requests")
	}
	if first == second {
		t.Error("the same server was handed to two requests; authority must be built per request")
	}
}

// Documents the cost this cache exists to avoid, and keeps the number
// reproducible rather than a figure someone measured once.
//
//	BenchmarkMCPServerConstruction/cached      ~0.5ms
//	BenchmarkMCPServerConstruction/uncached    ~2.2ms
func BenchmarkMCPServerConstruction(b *testing.B) {
	r := httptest.NewRequest("POST", "/mcp", nil)

	b.Run("cached", func(b *testing.B) {
		h := newBenchHandler(b)
		for i := 0; i < b.N; i++ {
			if _, err := h.mcpServerFor(r); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("uncached", func(b *testing.B) {
		h := newBenchHandler(b)
		h.schemas = nil
		for i := 0; i < b.N; i++ {
			if _, err := h.mcpServerFor(r); err != nil {
				b.Fatal(err)
			}
		}
	})
}
