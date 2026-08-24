package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// daemonAt serves 200 on exactly the given paths and 404 everywhere else, so a
// probe that accepts any response at all is visibly wrong rather than lucky.
func daemonAt(t *testing.T, paths ...string) *httptest.Server {
	t.Helper()
	ok := map[string]bool{}
	for _, p := range paths {
		ok[p] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ok[r.URL.Path] {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A 404 is a reachable web server, not a memstored. The old probe asked for
// /healthz -- a path memstored has never served -- and counted the 404 as a
// detection, so it "worked" only because anything listening on the port passed.
func TestCheckHTTP_RejectsANonSuccessStatus(t *testing.T) {
	srv := daemonAt(t, "/memstore/v1/health")

	if checkHTTP(srv.URL+"/healthz", 2*time.Second) {
		t.Error("a 404 counted as a healthy daemon")
	}
	if !checkHTTP(srv.URL+"/memstore/v1/health", 2*time.Second) {
		t.Error("a 200 did not count as a healthy daemon")
	}
}

// The prefix wins. A daemon serving both -- which every daemon does while the
// root alias stands -- must be addressed at its prefix, or the cutover never
// happens for anyone who ran setup during the transition.
func TestProbeDaemon_PrefersThePrefix(t *testing.T) {
	srv := daemonAt(t, "/memstore/v1/health", "/v1/health")

	got, ok := probeDaemon(srv.URL)
	if !ok {
		t.Fatal("daemon not detected")
	}
	if want := srv.URL + "/memstore"; got != want {
		t.Errorf("probeDaemon = %q, want %q", got, want)
	}
}

// A daemon that predates the prefix is still found, at the root.
func TestProbeDaemon_FallsBackToTheRoot(t *testing.T) {
	srv := daemonAt(t, "/v1/health")

	got, ok := probeDaemon(srv.URL)
	if !ok {
		t.Fatal("an unprefixed daemon was not detected")
	}
	if got != srv.URL {
		t.Errorf("probeDaemon = %q, want %q", got, srv.URL)
	}
}

// Probing a URL that already names the prefix must not append it twice, so
// re-running setup against a cut-over config is a no-op rather than a break.
func TestProbeDaemon_IsIdempotentOnAPrefixedURL(t *testing.T) {
	srv := daemonAt(t, "/memstore/v1/health", "/v1/health")

	got, ok := probeDaemon(srv.URL + "/memstore")
	if !ok {
		t.Fatal("a prefixed URL was not detected")
	}
	if want := srv.URL + "/memstore"; got != want {
		t.Errorf("probeDaemon = %q, want %q", got, want)
	}
}

func TestProbeDaemon_NothingListening(t *testing.T) {
	if got, ok := probeDaemon("http://127.0.0.1:1"); ok {
		t.Errorf("probeDaemon on a closed port returned %q", got)
	}
}

// The MCP endpoint is derived from the detected base, so it inherits whichever
// mount point was found rather than hardcoding one.
func TestMCPEndpointURL(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"http://host:8230/memstore", "http://host:8230/memstore/mcp"},
		{"http://host:8230", "http://host:8230/mcp"},
		{"http://host:8230/memstore/", "http://host:8230/memstore/mcp"},
	} {
		if got := mcpEndpointURL(tc.base); got != tc.want {
			t.Errorf("mcpEndpointURL(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

// A stale stdio registration is the normal state of a machine being cut over,
// and it must be recognised as stale rather than counted as "already done" --
// otherwise setup reports success and changes nothing.
func TestMCPRegistrationState(t *testing.T) {
	const want = "http://cube:8230/memstore/mcp"

	tests := []struct {
		name              string
		list              string
		registered, isCur bool
	}{
		{"absent", "github: /usr/bin/gh-mcp - Connected\n", false, false},
		{"stale stdio", "memstore: /home/m/go/bin/memstore-mcp --remote http://cube:8230 - Connected\n", true, false},
		{"stale http at the root", "memstore: http://cube:8230/mcp (HTTP) - Connected\n", true, false},
		{"current", "memstore: " + want + " (HTTP) - Connected\n", true, true},
		{"current among others", "a: x\nmemstore: " + want + " (HTTP)\nb: y\n", true, true},
		// A different server whose name merely contains ours is not ours.
		{"lookalike name", "memstore-dev: " + want + " (HTTP)\n", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			registered, isCur := mcpRegistrationState(tc.list, "memstore", want)
			if registered != tc.registered || isCur != tc.isCur {
				t.Errorf("got (registered=%v, current=%v), want (%v, %v)",
					registered, isCur, tc.registered, tc.isCur)
			}
		})
	}
}

// The live cutover case: a config that already names the daemon at the root has
// to move to the prefix, and a hand-edited TOML file must survive it intact.
func TestRewriteRemote(t *testing.T) {
	tests := []struct {
		name, in, want string
		changed        bool
	}{
		{
			name:    "rewrites the remote in place",
			in:      "# memstore configuration\nremote = \"http://cube:8230\"\napi_key = \"secret\"\n",
			want:    "# memstore configuration\nremote = \"http://cube:8230/memstore\"\napi_key = \"secret\"\n",
			changed: true,
		},
		{
			name:    "leaves an already-current remote alone",
			in:      "remote = \"http://cube:8230/memstore\"\n",
			want:    "remote = \"http://cube:8230/memstore\"\n",
			changed: false,
		},
		{
			name:    "tolerates whitespace and single quotes",
			in:      "  remote   =  'http://cube:8230'  \n",
			want:    "  remote = \"http://cube:8230/memstore\"\n",
			changed: true,
		},
		{
			name:    "uncomments nothing and adds nothing",
			in:      "# remote = \"http://localhost:8230\"\n",
			want:    "# remote = \"http://localhost:8230\"\n",
			changed: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := rewriteRemote(tc.in, "http://cube:8230/memstore")
			if got != tc.want || changed != tc.changed {
				t.Errorf("rewriteRemote:\n got %q (changed=%v)\nwant %q (changed=%v)", got, changed, tc.want, tc.changed)
			}
		})
	}
}
