package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matthewjhunter/memstore/httpapi"
)

// The API serves the same routes under the prefix as at the root, without
// knowing which one it was reached through.
func TestMount_ServesUnderThePrefixAndAtTheRoot(t *testing.T) {
	h, _ := newTestHandler(t)
	srv := httptest.NewServer(httpapi.Mount(httpapi.DefaultPrefix, h))
	defer srv.Close()

	for _, path := range []string{"/memstore/v1/health", "/v1/health"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", path, resp.StatusCode)
		}
	}
}

// The bare prefix redirects into its subtree rather than falling through to the
// root alias, which is what a person typing the base URL will hit. Where it
// lands is a separate question -- the API has no index route, so following the
// redirect legitimately 404s; what matters is that /memstore is not quietly
// answered by the root-mounted copy.
func TestMount_BarePrefixRedirectsIntoTheSubtree(t *testing.T) {
	h, _ := newTestHandler(t)
	srv := httptest.NewServer(httpapi.Mount(httpapi.DefaultPrefix, h))
	defer srv.Close()

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(srv.URL + "/memstore")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("status = %d, want 301", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/memstore/" {
		t.Errorf("Location = %q, want /memstore/", got)
	}
}

// /.well-known/ belongs to the host. Without the reservation the root alias
// answers for it, which would make the first module to serve a well-known
// document collide with the API and with every module after it.
func TestMount_WellKnownIsReservedForTheHost(t *testing.T) {
	h, _ := newTestHandler(t)
	srv := httptest.NewServer(httpapi.Mount(httpapi.DefaultPrefix, h))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/.well-known/oauth-protected-resource/memstore/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("well-known status = %d, want 404 from the host rather than an API answer", resp.StatusCode)
	}
}
