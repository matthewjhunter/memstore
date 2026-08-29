package memstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// ExportData is the top-level structure for a memstore export.
type ExportData struct {
	Version             int            `json:"version"`
	ExportedAt          time.Time      `json:"exported_at"`
	EmbedderModel       string         `json:"embedder_model,omitempty"`
	EmbeddingDimensions int            `json:"embedding_dimensions,omitempty"`
	Facts               []ExportedFact `json:"facts"`
	// Links is the edge set over Facts, keyed by the exported fact ids.
	// Absent from exports written before links travelled; an importer
	// treats absence as no links.
	Links []ExportedLink `json:"links,omitempty"`
}

// ExportedFact represents a single fact in an export. Embeddings are
// deliberately excluded -- they're model-specific binary blobs that don't
// transfer portably. Re-embed after import via EmbedFacts().
type ExportedFact struct {
	ID              int64           `json:"id"`
	Namespace       string          `json:"namespace"`
	User            string          `json:"user,omitempty"` // owner name from memstore_users; empty means import uses target default
	Content         string          `json:"content"`
	Subject         string          `json:"subject"`
	Category        string          `json:"category"`
	Kind            string          `json:"kind,omitempty"`
	Subsystem       string          `json:"subsystem,omitempty"`
	Metadata        json.RawMessage `json:"metadata,omitempty"`
	SupersededBy    *int64          `json:"superseded_by,omitempty"`
	SupersededAt    *time.Time      `json:"superseded_at,omitempty"`
	ConfirmedCount  int             `json:"confirmed_count,omitempty"`
	LastConfirmedAt *time.Time      `json:"last_confirmed_at,omitempty"`
	UseCount        int             `json:"use_count,omitempty"`
	LastUsedAt      *time.Time      `json:"last_used_at,omitempty"`
	InjectCount     int             `json:"inject_count,omitempty"`
	LastInjectedAt  *time.Time      `json:"last_injected_at,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
}

// ExportedLink is one edge of the link graph. SourceID and TargetID refer to
// ExportedFact.ID values in the same export and are remapped on import.
type ExportedLink struct {
	ID            int64           `json:"id"`
	Namespace     string          `json:"namespace"`
	SourceID      int64           `json:"source_id"`
	TargetID      int64           `json:"target_id"`
	LinkType      string          `json:"link_type"`
	Bidirectional bool            `json:"bidirectional,omitempty"`
	Label         string          `json:"label,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

// Export reads all facts (all namespaces, including superseded) and all
// links from the database and returns them as an ExportData struct. The
// database is a SQLite file written by memstore 0.5.x or earlier.
func Export(ctx context.Context, db *sql.DB) (*ExportData, error) {
	data := &ExportData{
		Version:    1,
		ExportedAt: time.Now().UTC(),
	}

	// Read embedder metadata if present. Errors are non-fatal — the
	// meta table may not exist in older schemas.
	var model string
	if db.QueryRowContext(ctx, `SELECT value FROM memstore_meta WHERE key = 'embedding_model'`).Scan(&model) == nil {
		data.EmbedderModel = model
	}
	var dimStr string
	if db.QueryRowContext(ctx, `SELECT value FROM memstore_meta WHERE key = 'embedding_dim'`).Scan(&dimStr) == nil {
		if d, err := strconv.Atoi(dimStr); err == nil {
			data.EmbeddingDimensions = d
		}
	}

	rows, err := db.QueryContext(ctx,
		`SELECT f.id, f.namespace, COALESCE(u.name, ''), f.content, f.subject, f.category, f.kind, f.subsystem, f.metadata,
		        f.superseded_by, f.superseded_at, f.confirmed_count, f.last_confirmed_at,
		        f.use_count, f.last_used_at, f.inject_count, f.last_injected_at, f.created_at
		 FROM memstore_facts f
		 LEFT JOIN memstore_users u ON u.id = f.user_id
		 ORDER BY f.id`)
	if err != nil {
		return nil, fmt.Errorf("memstore export: querying facts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ef ExportedFact
		var metadata sql.NullString
		var supersededBy *int64
		var supersededAt sql.NullString
		var lastConfirmedAt sql.NullString
		var lastUsedAt sql.NullString
		var lastInjectedAt sql.NullString
		var createdAt string

		if err := rows.Scan(&ef.ID, &ef.Namespace, &ef.User, &ef.Content, &ef.Subject, &ef.Category,
			&ef.Kind, &ef.Subsystem, &metadata, &supersededBy, &supersededAt,
			&ef.ConfirmedCount, &lastConfirmedAt,
			&ef.UseCount, &lastUsedAt,
			&ef.InjectCount, &lastInjectedAt, &createdAt); err != nil {
			return nil, fmt.Errorf("memstore export: scanning fact: %w", err)
		}

		if metadata.Valid && metadata.String != "" {
			ef.Metadata = json.RawMessage(metadata.String)
		}
		ef.SupersededBy = supersededBy
		if supersededAt.Valid {
			t, _ := time.Parse(time.RFC3339, supersededAt.String)
			ef.SupersededAt = &t
		}
		if lastConfirmedAt.Valid {
			t, _ := time.Parse(time.RFC3339, lastConfirmedAt.String)
			ef.LastConfirmedAt = &t
		}
		if lastUsedAt.Valid {
			t, _ := time.Parse(time.RFC3339, lastUsedAt.String)
			ef.LastUsedAt = &t
		}
		if lastInjectedAt.Valid {
			t, _ := time.Parse(time.RFC3339, lastInjectedAt.String)
			ef.LastInjectedAt = &t
		}
		ef.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)

		data.Facts = append(data.Facts, ef)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memstore export: iterating facts: %w", err)
	}

	links, err := exportLinks(ctx, db)
	if err != nil {
		return nil, err
	}
	data.Links = links
	return data, nil
}

func exportLinks(ctx context.Context, db *sql.DB) ([]ExportedLink, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, namespace, source_id, target_id, link_type, bidirectional, label, metadata, created_at
		 FROM memstore_links ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("memstore export: querying links: %w", err)
	}
	defer rows.Close()

	var out []ExportedLink
	for rows.Next() {
		var el ExportedLink
		var bidi int
		var metadata sql.NullString
		var createdAt string
		if err := rows.Scan(&el.ID, &el.Namespace, &el.SourceID, &el.TargetID, &el.LinkType,
			&bidi, &el.Label, &metadata, &createdAt); err != nil {
			return nil, fmt.Errorf("memstore export: scanning link: %w", err)
		}
		el.Bidirectional = bidi != 0
		if metadata.Valid && metadata.String != "" {
			el.Metadata = json.RawMessage(metadata.String)
		}
		el.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		out = append(out, el)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memstore export: iterating links: %w", err)
	}
	return out, nil
}

// ImportOpts controls import behavior.
type ImportOpts struct {
	// If true, skip facts whose (content, subject, namespace) already
	// exist in the target database.
	SkipDuplicates bool
}

// ImportResult summarizes an import operation.
type ImportResult struct {
	Imported int
	Skipped  int
	// Links is the number of edges recreated on the new ids. LinksSkipped
	// counts edges with an endpoint that did not land -- skipped as a
	// duplicate -- and so had nothing to attach to.
	Links        int
	LinksSkipped int
}

// linkMetadata decodes an exported link's metadata into the map LinkFacts
// takes. Absent or empty metadata is nil.
func linkMetadata(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// StoreImport inserts facts and links from an ExportData into a Store. Unlike
// Import, which uses raw SQL, this works through the Store interface and can
// target any backend (SQLite, Postgres, HTTP). Supersession chains are
// restored via Supersede() and links via LinkFacts() on the new ids.
// Use_count, confirmed_count, and timestamps other than a fact's created_at
// -- including a link's -- are not preserved (the Store interface doesn't
// expose setters).
func StoreImport(ctx context.Context, store Store, data *ExportData, opts ImportOpts) (*ImportResult, error) {
	if data.Version != 1 {
		return nil, fmt.Errorf("memstore store-import: unsupported export version %d", data.Version)
	}

	result := &ImportResult{}
	idMap := make(map[int64]int64) // oldID -> newID

	// First pass: insert all facts.
	for _, ef := range data.Facts {
		if opts.SkipDuplicates {
			exists, err := store.Exists(ctx, ef.Content, ef.Subject)
			if err != nil {
				return nil, fmt.Errorf("memstore store-import: checking duplicate: %w", err)
			}
			if exists {
				result.Skipped++
				continue
			}
		}

		newID, err := store.Insert(ctx, Fact{
			Content:   ef.Content,
			Subject:   ef.Subject,
			Category:  ef.Category,
			Kind:      ef.Kind,
			Subsystem: ef.Subsystem,
			Metadata:  ef.Metadata,
			CreatedAt: ef.CreatedAt,
		})
		if err != nil {
			return nil, fmt.Errorf("memstore store-import: inserting fact %d: %w", ef.ID, err)
		}

		idMap[ef.ID] = newID
		result.Imported++
	}

	// Second pass: restore supersession chains.
	for _, ef := range data.Facts {
		if ef.SupersededBy == nil {
			continue
		}
		oldNewID, ok := idMap[ef.ID]
		if !ok {
			continue
		}
		supersededByNewID, ok := idMap[*ef.SupersededBy]
		if !ok {
			continue
		}
		if err := store.Supersede(ctx, oldNewID, supersededByNewID); err != nil {
			return nil, fmt.Errorf("memstore store-import: superseding %d -> %d: %w",
				ef.ID, *ef.SupersededBy, err)
		}
	}

	// Third pass: links. An edge whose endpoint was skipped as a duplicate
	// has nothing to attach to and is counted rather than failed.
	for _, el := range data.Links {
		src, ok := idMap[el.SourceID]
		if !ok {
			result.LinksSkipped++
			continue
		}
		tgt, ok := idMap[el.TargetID]
		if !ok {
			result.LinksSkipped++
			continue
		}
		meta, err := linkMetadata(el.Metadata)
		if err != nil {
			return nil, fmt.Errorf("memstore store-import: link %d metadata: %w", el.ID, err)
		}
		if _, err := store.LinkFacts(ctx, src, tgt, el.LinkType, el.Bidirectional, el.Label, meta); err != nil {
			return nil, fmt.Errorf("memstore store-import: linking %d -> %d: %w", el.SourceID, el.TargetID, err)
		}
		result.Links++
	}

	return result, nil
}
