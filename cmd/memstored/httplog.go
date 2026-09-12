package main

import (
	"net/http"
	"strings"

	"github.com/matthewjhunter/memstore/httpapi"
)

// withIdentitySink leaves a slot on every request's context for the auth layer
// to record the caller in, so middleware wrapping the handler from outside --
// the access log -- can name them. See httpapi.WithIdentitySink.
func withIdentitySink(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, _ := httpapi.WithIdentitySink(r.Context())
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
