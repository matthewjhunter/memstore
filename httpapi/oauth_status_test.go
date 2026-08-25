package httpapi_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matthewjhunter/memstore/httpapi"
)

// failingVerifier returns a fixed error for every token, so the dispatch layer's
// mapping from verifier error to status code can be exercised on its own.
type failingVerifier struct{ err error }

func (v failingVerifier) VerifyToken(context.Context, string) (httpapi.Identity, error) {
	return httpapi.Identity{}, v.err
}

// The whole point of ErrAuthUnavailable is the status code it produces. That
// distinction is made in ServeHTTP, which previously answered 401 to every
// verifier error -- so without this test the sentinel could be threaded all the
// way through the adapter and still be flattened at the last step.
func TestServeHTTPDistinguishesUnavailableAuthFromBadCredentials(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{
			name: "a rejected token is the caller's problem",
			err:  errors.New("no such token"),
			want: http.StatusUnauthorized,
		},
		{
			name: "an unavailable authorization server is ours",
			err:  httpapi.ErrAuthUnavailable,
			want: http.StatusServiceUnavailable,
		},
		{
			// The sentinel is wrapped in practice, never returned bare.
			name: "and stays ours through wrapping",
			err:  fmt.Errorf("%w: fetching keys: connection refused", httpapi.ErrAuthUnavailable),
			want: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHandlerWith(t, httpapi.WithTokenVerifier(failingVerifier{err: tt.err}))
			req := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
			req.Header.Set("Authorization", "Bearer anything")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}
