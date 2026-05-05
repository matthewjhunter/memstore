// Package memstore provides a shared fact/knowledge store with FTS5 hybrid
// search and vector embeddings, backed by SQLite. The caller provides the
// *sql.DB; memstore creates its own namespaced tables (memstore_*).
//
// # Embedder
//
// memstore relies on github.com/matthewjhunter/go-embedding for the
// Embedder interface and embedding helpers (Single, EmbedWithRetry,
// CosineSimilarity, EncodeFloat32s, DecodeFloat32s). Construct an embedder
// with embedding.New(cfg) or embedding.New(embedding.ConfigFromEnvPrefix("MEMSTORE_EMBED")).
//
// # Conventions
//
// Relationship facts are directional: a fact like "Alice trusts Bob" with
// Subject "Alice" is only indexed under Alice. Searching for "Bob" depends
// on FTS matching "Bob" in the content text, which is fragile.
//
// To ensure reliable lookup from either side of a relationship, store both
// directions at insert time:
//
//	{Content: "Alice trusts Bob",        Subject: "Alice", Category: "relationship"}
//	{Content: "Bob is trusted by Alice", Subject: "Bob",   Category: "relationship"}
//
// This gives each direction its own FTS entry and embedding, so searches
// from either subject work naturally. The caller controls the inverse
// phrasing, which varies by relationship type.
package memstore
