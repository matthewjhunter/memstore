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
}

// ExportedFact represents a single fact in an export. Embeddings are
// deliberately excluded — they're model-specific binary blobs that don't
// transfer portably. Re-embed after import via EmbedFacts().
type ExportedFact struct {
	ID           int64           `json:"id"`
	Namespace    string          `json:"namespace"`
	Content      string          `json:"content"`
	Subject      string          `json:"subject"`
	Category     string          `json:"category"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	SupersededBy *int64          `json:"superseded_by,omitempty"`
	SupersededAt *time.Time      `json:"superseded_at,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// Export reads all facts (all namespaces, including superseded) from the
// database and returns them as an ExportData struct. The database must
// have been initialized by NewSQLiteStore at least once.
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
		`SELECT id, namespace, content, subject, category, metadata,
		        superseded_by, superseded_at, created_at
		 FROM memstore_facts ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("memstore export: querying facts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ef ExportedFact
		var metadata sql.NullString
		var supersededBy *int64
		var supersededAt sql.NullString
		var createdAt string

		if err := rows.Scan(&ef.ID, &ef.Namespace, &ef.Content, &ef.Subject, &ef.Category,
			&metadata, &supersededBy, &supersededAt, &createdAt); err != nil {
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
		ef.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)

		data.Facts = append(data.Facts, ef)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memstore export: iterating facts: %w", err)
	}

	return data, nil
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
}

// Import inserts facts from an ExportData into the database. Facts are
// inserted with their original namespace, timestamps, and metadata.
// SupersededBy references are remapped to new IDs. Embeddings are not
// imported — call EmbedFacts() after import to regenerate them.
func Import(ctx context.Context, db *sql.DB, data *ExportData, opts ImportOpts) (*ImportResult, error) {
	if data.Version != 1 {
		return nil, fmt.Errorf("memstore import: unsupported export version %d", data.Version)
	}

	result := &ImportResult{}

	// Group facts by namespace so we create one store per namespace.
	// The store runs migrations and sets up FTS triggers.
	byNS := make(map[string][]ExportedFact)
	for _, ef := range data.Facts {
		byNS[ef.Namespace] = append(byNS[ef.Namespace], ef)
	}

	// oldID -> newID mapping for supersession chain remapping.
	idMap := make(map[int64]int64)

	// First pass: insert all facts without supersession info.
	for ns, facts := range byNS {
		store, err := NewSQLiteStore(db, nil, ns)
		if err != nil {
			return nil, fmt.Errorf("memstore import: creating store for namespace %q: %w", ns, err)
		}

		for _, ef := range facts {
			if opts.SkipDuplicates {
				exists, err := store.Exists(ctx, ef.Content, ef.Subject)
				if err != nil {
					return nil, fmt.Errorf("memstore import: checking duplicate: %w", err)
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
				Metadata:  ef.Metadata,
				CreatedAt: ef.CreatedAt,
			})
			if err != nil {
				return nil, fmt.Errorf("memstore import: inserting fact %d: %w", ef.ID, err)
			}

			idMap[ef.ID] = newID
			result.Imported++
		}
	}

	// Second pass: restore supersession chains using direct SQL to
	// preserve the original superseded_at timestamps.
	for _, ef := range data.Facts {
		if ef.SupersededBy == nil {
			continue
		}

		oldNewID, ok := idMap[ef.ID]
		if !ok {
			continue // skipped as duplicate
		}
		supersededByNewID, ok := idMap[*ef.SupersededBy]
		if !ok {
			continue // superseding fact was skipped
		}

		var supersededAt *string
		if ef.SupersededAt != nil {
			s := ef.SupersededAt.UTC().Format(time.RFC3339)
			supersededAt = &s
		} else {
			s := time.Now().UTC().Format(time.RFC3339)
			supersededAt = &s
		}

		_, err := db.ExecContext(ctx,
			`UPDATE memstore_facts SET superseded_by = ?, superseded_at = ?
			 WHERE id = ? AND superseded_by IS NULL`,
			supersededByNewID, supersededAt, oldNewID,
		)
		if err != nil {
			return nil, fmt.Errorf("memstore import: restoring supersession %d -> %d: %w",
				ef.ID, *ef.SupersededBy, err)
		}
	}

	return result, nil
}
