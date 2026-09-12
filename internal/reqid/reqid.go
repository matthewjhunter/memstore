// Package reqid mints and carries a per-request correlation id.
//
// It is what lets two lines about the same request be tied together. The
// daemon puts one on every request and hands it to the access log; anything
// logging mid-request can attach the same id.
//
// Inbound X-Request-Id headers are deliberately not honoured. The header is
// client-supplied, so believing it from an arbitrary peer lets a caller
// collide ids with another caller's request, or reuse one across thousands of
// them, and the correlation it exists for stops meaning anything. Accepting
// one from a trusted proxy is a later change, and belongs with the same
// trusted-peer list that governs X-Forwarded-For.
package reqid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

type ctxKey struct{}

// New returns a fresh id: 8 random bytes, hex encoded. Random rather than
// sequential because ids appear in logs that leave the host, and a counter
// would disclose the request rate.
func New() string {
	var b [8]byte
	// crypto/rand.Read never returns an error (it panics on a broken source).
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// NewContext returns a context carrying id.
func NewContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the id on ctx, or the empty string when there is none.
// Its signature suits httplog.WithRequestID directly.
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}
