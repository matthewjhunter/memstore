package main

import (
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/matthewjhunter/memstore/httpapi"
	"github.com/matthewjhunter/memstore/internal/reqid"
	"github.com/matthewjhunter/memstore/internal/timing"
)

// withRequestContext puts a correlation id and a phase recorder on every
// request. Both are read back by the access log after the handler returns:
// the id ties any other line about this request to its access line, and the
// recorder is where the store and the handlers leave their per-phase times.
//
// It wraps outside the access log, like the identity sink, because a context
// installed by a handler is not one the middleware around it can see.
func withRequestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := reqid.NewContext(r.Context(), reqid.New())
		ctx = timing.NewContext(ctx)
		ctx, _ = httpapi.WithIdentitySink(ctx)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// splitList parses a comma-separated flag value, dropping empty fields so that
// an unset flag, a trailing comma and stray spaces all mean the same thing.
func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// stderrOr returns w, or os.Stderr when w is nil, for the daemon's log
// destination. run() is handed a writer by its caller; main passes os.Stderr
// and tests pass io.Discard.
func stderrOr(w io.Writer) io.Writer {
	if w == nil {
		return os.Stderr
	}
	return w
}
