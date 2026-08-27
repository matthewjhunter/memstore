package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matthewjhunter/memstore/httpapi"
)

// The API is reachable under the prefix and nowhere else. The transition
// alias that also served it at the root is gone: every client addresses the
// prefix now, and a root that still answered would mean the prefix bought
// nothing.
func TestMount_ServesUnderThePrefixOnly(t *testing.T) {
	h, _ := newTestHandler(t)
	srv := httptest.NewServer(httpapi.Mount(httpapi.DefaultPrefix, h))
	defer srv.Close()

	want := map[string]int{
		"/memstore/v1/health": http.StatusOK,
		"/v1/health":          http.StatusNotFound,
		"/mcp":                http.StatusNotFound,
	}
	for path, status := range want {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != status {
			t.Errorf("GET %s: status = %d, want %d", path, resp.StatusCode, status)
		}
	}
}

// The bare prefix redirects into its subtree, which is what a person typing the
// base URL will hit. Where it lands is a separate question -- the API has no
// index route, so following the redirect legitimately 404s; what matters is
// that /memstore is answered by the mount and not by a fallthrough.
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

// /.well-known/ belongs to the host, so an unconfigured well-known document is
// the host's 404 and never an API answer. The reservation is what keeps the
// first module to serve one from colliding with every module after it.
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
