package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matthewjhunter/memstore/httpapi"
)

// An access log wraps the handler from outside, so it cannot see the context
// the auth middleware builds: WithIdentity returns a derived context that only
// the inner handler chain ever holds. The sink is the one channel back out.
func TestIdentitySinkRecordsTheAuthenticatedName(t *testing.T) {
	ctx, identity := httpapi.WithIdentitySink(context.Background())
	if got := identity(); got != "" {
		t.Errorf("an unauthenticated request should report no identity, got %q", got)
	}

	// What the auth middleware does, one derived context deeper.
	inner := httpapi.WithIdentity(ctx, httpapi.Identity{Name: "matthew-laptop", Source: "bearer"})
	_ = inner

	if got := identity(); got != "matthew-laptop" {
		t.Errorf("identity() = %q, want %q", got, "matthew-laptop")
	}
}

func TestIdentitySinkLastWriterWins(t *testing.T) {
	ctx, identity := httpapi.WithIdentitySink(context.Background())
	httpapi.WithIdentity(ctx, httpapi.Identity{Name: "first"})
	httpapi.WithIdentity(ctx, httpapi.Identity{Name: "legacy", Source: "legacy"})
	if got := identity(); got != "legacy" {
		t.Errorf("identity() = %q, want the most recent %q", got, "legacy")
	}
}

// WithIdentity must stay usable on a context that carries no sink: most of its
// callers are tests and handlers that never go through the access log.
func TestWithIdentityWithoutSink(t *testing.T) {
	ctx := httpapi.WithIdentity(context.Background(), httpapi.Identity{Name: "nobody"})
	if id, ok := httpapi.IdentityFromContext(ctx); !ok || id.Name != "nobody" {
		t.Errorf("IdentityFromContext = %v, %v; want the identity back", id, ok)
	}
}

// The sink is written by the handler goroutine and read by the middleware
// after ServeHTTP returns; a hijacked or streaming handler can still be
// writing. -race proves the lock.
func TestIdentitySinkConcurrentAccess(t *testing.T) {
	ctx, identity := httpapi.WithIdentitySink(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			httpapi.WithIdentity(ctx, httpapi.Identity{Name: "writer"})
		}
	}()
	for i := 0; i < 100; i++ {
		_ = identity()
	}
	<-done
}

// End to end through the real handler: the name the token verifier resolved
// reaches an outer wrapper.
func TestIdentitySinkThroughHandler(t *testing.T) {
	h := newScopeHandler(t)

	var seen string
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, identity := httpapi.WithIdentitySink(r.Context())
		h.ServeHTTP(w, r.WithContext(ctx))
		seen = identity()
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer tok-read")
	outer.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "tok-read" {
		t.Errorf("identity seen by the outer wrapper = %q, want %q", seen, "tok-read")
	}
}

func TestSinkIdentityNameFromContext(t *testing.T) {
	if got := httpapi.SinkIdentityName(context.Background()); got != "" {
		t.Errorf("a context with no sink should report no identity, got %q", got)
	}
	ctx, _ := httpapi.WithIdentitySink(context.Background())
	if got := httpapi.SinkIdentityName(ctx); got != "" {
		t.Errorf("an unauthenticated request should report no identity, got %q", got)
	}
	httpapi.WithIdentity(ctx, httpapi.Identity{Name: "matthew-laptop"})
	if got := httpapi.SinkIdentityName(ctx); got != "matthew-laptop" {
		t.Errorf("SinkIdentityName = %q, want %q", got, "matthew-laptop")
	}
}
