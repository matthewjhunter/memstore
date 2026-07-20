package pgstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/matthewjhunter/memstore"
)

// This file implements memstore.DocumentStore: the verbatim document corpus
// from docs/document-corpus.md. Documents and chunks are written only by the
// ingest path (never by model-facing tools); user_id and namespace are
// denormalized onto chunks so the isolation predicate never depends on a join.

// PostgresStore carries the document corpus.
var _ memstore.DocumentStore = (*PostgresStore)(nil)

// docColumns is the canonical SELECT list for document queries.
const docColumns = `id, namespace, user_id, repo_url, commit, path, basename, lang, file_sha256, mtime, dirty, trusted, generated, is_test, chunker_version, title, front_matter, ingested_at`

// docChunkColumns is the canonical SELECT list for chunk queries. The search
// path has its own list because it prefixes with c. and joins document
// identity columns.
const docChunkColumns = `id, document_id, ordinal, content, byte_start, byte_end, line_start, line_end, heading_path, heading_level, lang, package, import_path, symbol, receiver, decl_kind, exported, signature, scope_path, imports_used, created_at`

// migrateV5 creates the document corpus tables (docs/document-corpus.md,
// docs/document-chunking.md, docs/code-chunking.md).
//
// The chunk fts column implements the measured decomposed-with-fallback
// design from docs/embedding-model-routing.md: the exact english tsvector at
// weight B, plus a decomposed form -- camelCase boundaries and / . _ - :
// separators split apart, 'simple' config so nothing is stemmed or
// stopworded -- at weight D. Documents are always decomposed so the tokens
// exist; queries decompose only on fallback.
//
// Loose files (repo_url IS NULL) need their own partial unique index because
// Postgres treats NULLs as distinct in the main identity index -- without it,
// loose-file re-ingest accumulates rows instead of replacing
// (docs/document-ingest.md).
func (s *PostgresStore) migrateV5(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS memstore_documents (
			id              BIGSERIAL PRIMARY KEY,
			namespace       TEXT NOT NULL,
			user_id         BIGINT NOT NULL REFERENCES memstore_users(id),
			repo_url        TEXT,
			commit          TEXT NOT NULL DEFAULT '',
			path            TEXT NOT NULL CHECK (path <> ''),
			basename        TEXT NOT NULL,
			lang            TEXT NOT NULL DEFAULT '',
			file_sha256     BYTEA NOT NULL CHECK (length(file_sha256) = 32),
			mtime           TIMESTAMPTZ,
			dirty           BOOLEAN NOT NULL DEFAULT FALSE,
			trusted         BOOLEAN NOT NULL DEFAULT FALSE,
			generated       BOOLEAN NOT NULL DEFAULT FALSE,
			is_test         BOOLEAN NOT NULL DEFAULT FALSE,
			chunker_version INTEGER NOT NULL,
			title           TEXT NOT NULL DEFAULT '',
			front_matter    JSONB,
			ingested_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_memstore_documents_identity
			ON memstore_documents (namespace, user_id, repo_url, path)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_memstore_documents_loose
			ON memstore_documents (namespace, user_id, path) WHERE repo_url IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_documents_user
			ON memstore_documents (namespace, user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_documents_basename
			ON memstore_documents (namespace, user_id, basename)`,

		`CREATE TABLE IF NOT EXISTS memstore_document_chunks (
			id            BIGSERIAL PRIMARY KEY,
			namespace     TEXT NOT NULL,
			user_id       BIGINT NOT NULL REFERENCES memstore_users(id),
			document_id   BIGINT NOT NULL REFERENCES memstore_documents(id) ON DELETE CASCADE,
			ordinal       INTEGER NOT NULL,
			content       TEXT NOT NULL CHECK (octet_length(content) <= 65536),
			byte_start    INTEGER NOT NULL,
			byte_end      INTEGER NOT NULL,
			line_start    INTEGER NOT NULL,
			line_end      INTEGER NOT NULL,
			heading_path  TEXT NOT NULL DEFAULT '',
			heading_level INTEGER NOT NULL DEFAULT 0,
			lang          TEXT NOT NULL DEFAULT '',
			package       TEXT NOT NULL DEFAULT '',
			import_path   TEXT NOT NULL DEFAULT '',
			symbol        TEXT NOT NULL DEFAULT '',
			receiver      TEXT NOT NULL DEFAULT '',
			decl_kind     TEXT NOT NULL DEFAULT '',
			exported      BOOLEAN NOT NULL DEFAULT FALSE,
			signature     TEXT NOT NULL DEFAULT '',
			scope_path    TEXT NOT NULL DEFAULT '',
			imports_used  TEXT[],
			fts           TSVECTOR GENERATED ALWAYS AS (
				setweight(to_tsvector('english', content), 'B') ||
				setweight(to_tsvector('simple',
					regexp_replace(
						regexp_replace(
							translate(content, '/._-:', '     '),
							'([a-z0-9])([A-Z])', '\1 \2', 'g'),
						'([A-Z]+)([A-Z][a-z])', '\1 \2', 'g')
				), 'D')
			) STORED,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (document_id, ordinal),
			CHECK (byte_end > byte_start)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_document_chunks_fts
			ON memstore_document_chunks USING GIN (fts)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_document_chunks_doc
			ON memstore_document_chunks (document_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memstore_document_chunks_user
			ON memstore_document_chunks (namespace, user_id)`,
	}
	for _, stmt := range stmts {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("pgstore V5 migration: %w\nstatement: %s", err, stmt)
		}
	}
	return nil
}

// docOwnerFor resolves the user_id an incoming document should be written
// under -- the DocumentStore analogue of ownerFor, with the same contract: a
// scoped store writes its own user's documents and no one else's, a mismatch
// is rejected rather than silently corrected, and service scope may write for
// any user but must name one.
func (s *PostgresStore) docOwnerFor(d memstore.Document) (int64, error) {
	if s.userID != 0 {
		if d.UserID != 0 && d.UserID != s.userID {
			return 0, fmt.Errorf(
				"pgstore: document carries user_id %d but this store is scoped to user %d: a scoped store cannot write another user's documents",
				d.UserID, s.userID)
		}
		return s.userID, nil
	}
	if d.UserID == 0 {
		return 0, errors.New("pgstore: service-scope document upsert requires an explicit Document.UserID")
	}
	return d.UserID, nil
}

// validateDocumentChunks enforces the span invariants from
// docs/document-chunking.md before anything is written: ordinals dense from
// zero, spans non-empty, non-overlapping and monotonically increasing, and
// content length equal to the span it claims. Coverage of the whole file is
// deliberately NOT checked -- front matter and inter-block whitespace are
// skipped by design.
func validateDocumentChunks(chunks []memstore.DocumentChunk) error {
	prevEnd := 0
	for i, c := range chunks {
		if c.Ordinal != i {
			return fmt.Errorf("pgstore: chunk %d has ordinal %d; ordinals must be dense from zero", i, c.Ordinal)
		}
		if c.ByteEnd <= c.ByteStart {
			return fmt.Errorf("pgstore: chunk %d span [%d,%d) is empty or inverted", i, c.ByteStart, c.ByteEnd)
		}
		if c.ByteStart < prevEnd {
			return fmt.Errorf("pgstore: chunk %d span [%d,%d) overlaps the previous chunk (ends at %d)", i, c.ByteStart, c.ByteEnd, prevEnd)
		}
		if len(c.Content) != c.ByteEnd-c.ByteStart {
			return fmt.Errorf("pgstore: chunk %d content is %d bytes but its span [%d,%d) claims %d", i, len(c.Content), c.ByteStart, c.ByteEnd, c.ByteEnd-c.ByteStart)
		}
		if c.LineStart < 1 || c.LineEnd < c.LineStart {
			return fmt.Errorf("pgstore: chunk %d has invalid line span %d-%d", i, c.LineStart, c.LineEnd)
		}
		prevEnd = c.ByteEnd
	}
	return nil
}

// UpsertDocument stores or replaces a document and its chunks atomically.
// Replacement is keyed on (namespace, user, repo_url, path) -- commit is
// deliberately outside the key, so re-ingesting at a new commit replaces the
// row. The previous chunk set is deleted, never merged.
func (s *PostgresStore) UpsertDocument(ctx context.Context, doc memstore.Document, chunks []memstore.DocumentChunk) (int64, error) {
	owner, err := s.docOwnerFor(doc)
	if err != nil {
		return 0, err
	}
	if doc.Path == "" {
		return 0, errors.New("pgstore: document path must not be empty")
	}
	if len(doc.FileSHA256) != sha256.Size {
		return 0, fmt.Errorf("pgstore: file_sha256 must be %d bytes, got %d", sha256.Size, len(doc.FileSHA256))
	}
	if err := validateDocumentChunks(chunks); err != nil {
		return 0, err
	}

	// The conflict target must name the index that actually enforces
	// uniqueness for this identity: the loose-file case is covered by the
	// partial index, not the main one (NULLs are distinct there).
	conflict := `(namespace, user_id, repo_url, path)`
	if doc.RepoURL == "" {
		conflict = `(namespace, user_id, path) WHERE repo_url IS NULL`
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("pgstore: UpsertDocument: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var id int64
	err = tx.QueryRow(ctx,
		`INSERT INTO memstore_documents
			(namespace, user_id, repo_url, commit, path, basename, lang, file_sha256,
			 mtime, dirty, trusted, generated, is_test, chunker_version, title, front_matter)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		 ON CONFLICT `+conflict+` DO UPDATE SET
			commit = EXCLUDED.commit,
			lang = EXCLUDED.lang,
			file_sha256 = EXCLUDED.file_sha256,
			mtime = EXCLUDED.mtime,
			dirty = EXCLUDED.dirty,
			trusted = EXCLUDED.trusted,
			generated = EXCLUDED.generated,
			is_test = EXCLUDED.is_test,
			chunker_version = EXCLUDED.chunker_version,
			title = EXCLUDED.title,
			front_matter = EXCLUDED.front_matter,
			ingested_at = NOW()
		 RETURNING id`,
		s.namespace, owner, nullableText(doc.RepoURL), doc.Commit, doc.Path,
		path.Base(doc.Path), doc.Lang, doc.FileSHA256, doc.Mtime, doc.Dirty,
		doc.Trusted, doc.Generated, doc.IsTest, doc.ChunkerVersion, doc.Title,
		nullableJSON(doc.FrontMatter),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("pgstore: UpsertDocument: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM memstore_document_chunks WHERE document_id = $1`, id,
	); err != nil {
		return 0, fmt.Errorf("pgstore: UpsertDocument: clearing previous chunks: %w", err)
	}

	if len(chunks) > 0 {
		b := &pgx.Batch{}
		for _, c := range chunks {
			b.Queue(
				`INSERT INTO memstore_document_chunks
					(namespace, user_id, document_id, ordinal, content,
					 byte_start, byte_end, line_start, line_end,
					 heading_path, heading_level, lang,
					 package, import_path, symbol, receiver, decl_kind, exported,
					 signature, scope_path, imports_used)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)`,
				s.namespace, owner, id, c.Ordinal, c.Content,
				c.ByteStart, c.ByteEnd, c.LineStart, c.LineEnd,
				c.HeadingPath, c.HeadingLevel, c.Lang,
				c.Package, c.ImportPath, c.Symbol, c.Receiver, c.DeclKind, c.Exported,
				c.Signature, c.ScopePath, c.ImportsUsed,
			)
		}
		br := tx.SendBatch(ctx, b)
		for i := range chunks {
			if _, err := br.Exec(); err != nil {
				br.Close()
				return 0, fmt.Errorf("pgstore: UpsertDocument: inserting chunk %d: %w", i, err)
			}
		}
		if err := br.Close(); err != nil {
			return 0, fmt.Errorf("pgstore: UpsertDocument: closing chunk batch: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("pgstore: UpsertDocument: commit: %w", err)
	}
	return id, nil
}

// ListDocuments returns the manifest view of every stored document for a repo
// identity, ordered by path. repoURL "" selects loose files (repo_url IS
// NULL) -- unlike search, where "" means unfiltered, a manifest is always
// scoped to one identity.
func (s *PostgresStore) ListDocuments(ctx context.Context, repoURL string) ([]memstore.DocumentInfo, error) {
	q := `SELECT id, path, file_sha256, dirty FROM memstore_documents WHERE namespace = $1`
	args := []any{s.namespace}
	if repoURL == "" {
		q += ` AND repo_url IS NULL`
	} else {
		args = append(args, repoURL)
		q += fmt.Sprintf(` AND repo_url = $%d`, len(args))
	}
	q, args = s.userPredicate(q, args)
	q += ` ORDER BY path`

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: ListDocuments: %w", err)
	}
	defer rows.Close()

	var infos []memstore.DocumentInfo
	for rows.Next() {
		var info memstore.DocumentInfo
		if err := rows.Scan(&info.ID, &info.Path, &info.FileSHA256, &info.Dirty); err != nil {
			return nil, fmt.Errorf("pgstore: scanning document info: %w", err)
		}
		infos = append(infos, info)
	}
	return infos, rows.Err()
}

// DeleteDocuments removes the named documents for a repo identity; chunks
// cascade. Returns the number of documents actually deleted -- paths that were
// never stored (or belong to another user) simply do not count.
func (s *PostgresStore) DeleteDocuments(ctx context.Context, repoURL string, paths []string) (int64, error) {
	if len(paths) == 0 {
		return 0, nil
	}
	q := `DELETE FROM memstore_documents WHERE namespace = $1 AND path = ANY($2::text[])`
	args := []any{s.namespace, paths}
	if repoURL == "" {
		q += ` AND repo_url IS NULL`
	} else {
		args = append(args, repoURL)
		q += fmt.Sprintf(` AND repo_url = $%d`, len(args))
	}
	q, args = s.userPredicate(q, args)

	ct, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("pgstore: DeleteDocuments: %w", err)
	}
	return ct.RowsAffected(), nil
}

// GetDocument returns a document by id, or (nil, nil) when no document with
// that id is visible in the caller's scope.
func (s *PostgresStore) GetDocument(ctx context.Context, id int64) (*memstore.Document, error) {
	q := `SELECT ` + docColumns + ` FROM memstore_documents WHERE id = $1 AND namespace = $2`
	args := []any{id, s.namespace}
	q, args = s.userPredicate(q, args)

	doc, err := scanDocument(s.pool.QueryRow(ctx, q, args...))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pgstore: GetDocument: %w", err)
	}
	return doc, nil
}

// GetDocumentChunks returns a document's chunks ordered by ordinal. A
// document outside the caller's scope yields no chunks, matching
// GetDocument's not-found contract.
func (s *PostgresStore) GetDocumentChunks(ctx context.Context, documentID int64) ([]memstore.DocumentChunk, error) {
	q := `SELECT ` + docChunkColumns + ` FROM memstore_document_chunks WHERE document_id = $1 AND namespace = $2`
	args := []any{documentID, s.namespace}
	q, args = s.userPredicate(q, args)
	q += ` ORDER BY ordinal`

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: GetDocumentChunks: %w", err)
	}
	defer rows.Close()

	var chunks []memstore.DocumentChunk
	for rows.Next() {
		c, err := scanDocumentChunk(rows)
		if err != nil {
			return nil, fmt.Errorf("pgstore: scanning chunk: %w", err)
		}
		chunks = append(chunks, *c)
	}
	return chunks, rows.Err()
}

// SearchDocumentChunks is FTS over chunk content, per the measured design in
// docs/embedding-model-routing.md: an exact english pass first; then, only
// when the exact pass leaves room below MaxResults, a decomposed-identifier
// pass against the 'simple' weight-D lexemes, appended after the exact hits
// and marked Fallback. Appending rather than blending is the measured
// property: the fallback can only fill space an exact match was not using, so
// queries that already work keep their ranking.
func (s *PostgresStore) SearchDocumentChunks(ctx context.Context, query string, opts memstore.DocumentSearchOpts) ([]memstore.DocumentSearchResult, error) {
	if opts.MaxResults <= 0 {
		opts.MaxResults = 20
	}
	tsquery := quoteFTSQuery(query)
	if tsquery == "" {
		return nil, nil
	}

	results, err := s.searchDocChunks(ctx, "english", tsquery, opts, opts.MaxResults, nil)
	if err != nil {
		return nil, err
	}

	if room := opts.MaxResults - len(results); room > 0 {
		if decomposed := decomposeQuery(query); decomposed != "" && decomposed != strings.ToLower(strings.Join(strings.Fields(query), " ")) {
			exclude := make([]int64, 0, len(results))
			for _, r := range results {
				exclude = append(exclude, r.Chunk.ID)
			}
			fallback, err := s.searchDocChunks(ctx, "simple", quoteFTSQuery(decomposed), opts, room, exclude)
			if err != nil {
				return nil, err
			}
			for i := range fallback {
				fallback[i].Fallback = true
			}
			results = append(results, fallback...)
		}
	}
	return results, nil
}

// searchDocChunks runs one ranked FTS pass over chunks joined to their
// document identity. The join carries namespace and user_id on both sides:
// the chunk-side predicate is the isolation boundary (denormalized on
// purpose), the join condition keeps the document row honest.
func (s *PostgresStore) searchDocChunks(ctx context.Context, config, tsquery string, opts memstore.DocumentSearchOpts, limit int, excludeIDs []int64) ([]memstore.DocumentSearchResult, error) {
	var b queryBuilder
	b.write(`SELECT c.id, c.document_id, c.ordinal, c.content, c.byte_start, c.byte_end, c.line_start, c.line_end,
			c.heading_path, c.heading_level, c.lang,
			c.package, c.import_path, c.symbol, c.receiver, c.decl_kind, c.exported, c.signature, c.scope_path, c.imports_used,
			c.created_at,
			d.repo_url, d.commit, d.path, d.basename, d.lang, d.trusted, d.dirty, d.generated, d.is_test,
			ts_rank(c.fts, plainto_tsquery('`+config+`', `, tsquery)
	b.q += `)) AS rank
		FROM memstore_document_chunks c
		JOIN memstore_documents d
		  ON d.id = c.document_id AND d.namespace = c.namespace AND d.user_id = c.user_id
		WHERE c.fts @@ plainto_tsquery('` + config + `', `
	b.write(``, tsquery)
	b.q += `)`

	b.write(` AND c.namespace = `, s.namespace)
	s.appendUserFilter(&b, "c.user_id")
	if !opts.IncludeGenerated {
		b.q += ` AND NOT d.generated`
	}
	if opts.RepoURL != "" {
		b.write(` AND d.repo_url = `, opts.RepoURL)
	}
	if opts.PathPrefix != "" {
		b.write(` AND starts_with(d.path, `, opts.PathPrefix)
		b.q += `)`
	}
	if opts.Basename != "" {
		b.write(` AND d.basename = `, opts.Basename)
	}
	if opts.Lang != "" {
		b.write(` AND d.lang = `, opts.Lang)
	}
	if len(excludeIDs) > 0 {
		b.args = append(b.args, excludeIDs)
		b.q += fmt.Sprintf(` AND NOT (c.id = ANY($%d::bigint[]))`, len(b.args))
	}
	b.write(` ORDER BY rank DESC, c.document_id, c.ordinal LIMIT `, limit)

	rows, err := s.pool.Query(ctx, b.q, b.args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: document search: %w", err)
	}
	defer rows.Close()

	var results []memstore.DocumentSearchResult
	for rows.Next() {
		var r memstore.DocumentSearchResult
		var repoURL *string
		c := &r.Chunk
		err := rows.Scan(
			&c.ID, &c.DocumentID, &c.Ordinal, &c.Content, &c.ByteStart, &c.ByteEnd, &c.LineStart, &c.LineEnd,
			&c.HeadingPath, &c.HeadingLevel, &c.Lang,
			&c.Package, &c.ImportPath, &c.Symbol, &c.Receiver, &c.DeclKind, &c.Exported, &c.Signature, &c.ScopePath, &c.ImportsUsed,
			&c.CreatedAt,
			&repoURL, &r.Commit, &r.Path, &r.Basename, &r.DocLang, &r.Trusted, &r.Dirty, &r.Generated, &r.IsTest,
			&r.Score,
		)
		if err != nil {
			return nil, fmt.Errorf("pgstore: scanning document search result: %w", err)
		}
		if repoURL != nil {
			r.RepoURL = *repoURL
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// scanDocument scans one row of docColumns.
func scanDocument(row scanner) (*memstore.Document, error) {
	var d memstore.Document
	var repoURL *string
	var frontMatter []byte
	err := row.Scan(
		&d.ID, &d.Namespace, &d.UserID, &repoURL, &d.Commit, &d.Path, &d.Basename, &d.Lang,
		&d.FileSHA256, &d.Mtime, &d.Dirty, &d.Trusted, &d.Generated, &d.IsTest,
		&d.ChunkerVersion, &d.Title, &frontMatter, &d.IngestedAt,
	)
	if err != nil {
		return nil, err
	}
	if repoURL != nil {
		d.RepoURL = *repoURL
	}
	if len(frontMatter) > 0 {
		d.FrontMatter = frontMatter
	}
	return &d, nil
}

// scanDocumentChunk scans one row of docChunkColumns.
func scanDocumentChunk(row scanner) (*memstore.DocumentChunk, error) {
	var c memstore.DocumentChunk
	err := row.Scan(
		&c.ID, &c.DocumentID, &c.Ordinal, &c.Content, &c.ByteStart, &c.ByteEnd, &c.LineStart, &c.LineEnd,
		&c.HeadingPath, &c.HeadingLevel, &c.Lang,
		&c.Package, &c.ImportPath, &c.Symbol, &c.Receiver, &c.DeclKind, &c.Exported, &c.Signature, &c.ScopePath, &c.ImportsUsed,
		&c.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// nullableText maps "" to SQL NULL. Used for repo_url, where NULL is
// semantically distinct: it is what the loose-file partial unique index keys
// on.
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// docSeparatorReplacer mirrors the separator set in the migrateV5 fts
// expression: / . _ - : all become spaces.
var docSeparatorReplacer = strings.NewReplacer("/", " ", ".", " ", "_", " ", "-", " ", ":", " ")

// decomposeQuery applies the same decomposition to a query that migrateV5's
// fts expression applies to chunk content: separators to spaces, camelCase
// split at lower-to-upper and acronym boundaries, lowercased with whitespace
// collapsed. The two must stay in agreement -- query-side tokens that the
// document side never produced match nothing.
func decomposeQuery(q string) string {
	q = docSeparatorReplacer.Replace(q)
	runes := []rune(q)
	var b strings.Builder
	b.Grow(len(q) + 8)
	for i, r := range runes {
		if i > 0 {
			prev := runes[i-1]
			lowerToUpper := (unicode.IsLower(prev) || unicode.IsDigit(prev)) && unicode.IsUpper(r)
			acronymEnd := i+1 < len(runes) && unicode.IsUpper(prev) && unicode.IsUpper(r) && unicode.IsLower(runes[i+1])
			if lowerToUpper || acronymEnd {
				b.WriteByte(' ')
			}
		}
		b.WriteRune(r)
	}
	return strings.ToLower(strings.Join(strings.Fields(b.String()), " "))
}
