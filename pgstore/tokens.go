package pgstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TokenStore manages API tokens used by memstored for bearer-token auth.
// It is independent of PostgresStore — its tables are isolated.
//
// Tokens are stored as SHA-256 of the token string. The plaintext token is
// returned exactly once (from Issue) and never retrievable afterwards.
type TokenStore struct {
	pool *pgxpool.Pool
}

// TokenInfo describes a token row without revealing the token value.
type TokenInfo struct {
	ID         int64
	Name       string
	UserID     int64
	Scopes     []string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
}

// IssueOpts configures a new token.
type IssueOpts struct {
	UserID  int64         // required; Issue returns an error if zero
	Scopes  []string      // optional, e.g. {"read","write","admin"}
	Expires time.Duration // 0 = no expiry
}

// VerifyResult is what Verify returns for a valid token: enough to populate
// httpapi.Identity (kept here as primitives so this package doesn't import
// httpapi).
type VerifyResult struct {
	Name   string
	Scopes []string
	UserID int64
}

// ErrTokenInvalid is returned by Verify for any reason a token doesn't
// authenticate (not found, revoked, expired). Callers must not distinguish
// between these reasons in error messages — they all collapse to 401.
var ErrTokenInvalid = errors.New("token invalid")

// tokenPrefix is the format-recognition tag on memstore-issued tokens.
// Lets operators grep secrets out of logs and tells them at a glance
// what kind of secret they're looking at.
const tokenPrefix = "mst_"

// NewTokenStore creates a TokenStore and runs its migrations.
func NewTokenStore(ctx context.Context, pool *pgxpool.Pool) (*TokenStore, error) {
	s := &TokenStore{pool: pool}
	return s, s.migrate(ctx)
}

// tokenNameRe validates the <user>@<host> token name shape enforced at issuance.
var tokenNameRe = regexp.MustCompile(`^[a-z0-9_-]+@[a-z0-9_-]+$`)

// sanitizeName converts a token name to a safe identifier: lowercase,
// replace non-alphanumeric/underscore/hyphen with underscore.
func sanitizeName(s string) string {
	re := regexp.MustCompile(`[^a-z0-9_-]`)
	return re.ReplaceAllString(strings.ToLower(s), "_")
}

func (s *TokenStore) migrate(ctx context.Context) error {
	// Base schema (idempotent via IF NOT EXISTS).
	baseStmts := []string{
		`CREATE TABLE IF NOT EXISTS api_tokens (
			id           BIGSERIAL PRIMARY KEY,
			token_hash   BYTEA       NOT NULL UNIQUE,
			name         TEXT        NOT NULL,
			scopes       TEXT[]      NOT NULL DEFAULT '{}',
			created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
			last_used_at TIMESTAMPTZ,
			expires_at   TIMESTAMPTZ,
			revoked_at   TIMESTAMPTZ
		)`,
		`CREATE INDEX IF NOT EXISTS idx_api_tokens_active
		     ON api_tokens (token_hash) WHERE revoked_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_api_tokens_name
		     ON api_tokens (name) WHERE revoked_at IS NULL`,
	}
	for _, stmt := range baseStmts {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("token store migrate: %w\nstatement: %s", err, stmt)
		}
	}

	// Check whether memstore_users exists (store migration must precede token migration).
	var usersExist bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'memstore_users')`,
	).Scan(&usersExist); err != nil {
		return fmt.Errorf("token store migrate: checking memstore_users: %w", err)
	}
	if !usersExist {
		return fmt.Errorf("memstore_users table not found; run store migration before token migration")
	}

	// Check whether user_id column already exists on api_tokens.
	var colExists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			 WHERE table_name = 'api_tokens' AND column_name = 'user_id'
		)`,
	).Scan(&colExists); err != nil {
		return fmt.Errorf("token store migrate: checking user_id column: %w", err)
	}
	if colExists {
		return nil // already migrated
	}

	// Add user_id nullable first so we can backfill.
	if _, err := s.pool.Exec(ctx, `ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS user_id BIGINT`); err != nil {
		return fmt.Errorf("token store migrate: add user_id: %w", err)
	}

	// Check if there are any existing rows to backfill.
	var rowCount int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM api_tokens`).Scan(&rowCount); err != nil {
		return fmt.Errorf("token store migrate: counting tokens: %w", err)
	}

	if rowCount > 0 {
		// Look up or resolve the default user from memstore_meta.
		var defaultUser string
		if err := s.pool.QueryRow(ctx, `SELECT value FROM memstore_meta WHERE key = 'default_user'`).Scan(&defaultUser); err != nil {
			return fmt.Errorf("token store migrate: reading default_user from memstore_meta: %w. Run 'memstore admin tier3-init --default-user <name>' first", err)
		}

		// Resolve user_id from memstore_users.
		var defaultUID int64
		if err := s.pool.QueryRow(ctx,
			`SELECT id FROM memstore_users WHERE name = $1 LIMIT 1`, defaultUser,
		).Scan(&defaultUID); err != nil {
			return fmt.Errorf("token store migrate: resolving user %q in memstore_users: %w", defaultUser, err)
		}

		// Rewrite token names and backfill user_id.
		// Rules:
		//   "matthew-laptop" -> "matthew@laptop"
		//   "legacy"         -> "<defaultUser>@legacy"
		//   other            -> "<defaultUser>@<sanitized>" (with a logged warning)
		rows, err := s.pool.Query(ctx, `SELECT id, name FROM api_tokens`)
		if err != nil {
			return fmt.Errorf("token store migrate: reading token names: %w", err)
		}
		type tokenRow struct {
			id   int64
			name string
		}
		var tokens []tokenRow
		for rows.Next() {
			var r tokenRow
			if err := rows.Scan(&r.id, &r.name); err != nil {
				rows.Close()
				return fmt.Errorf("token store migrate: scanning token: %w", err)
			}
			tokens = append(tokens, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("token store migrate: iterating tokens: %w", err)
		}

		for _, tok := range tokens {
			var newName string
			if idx := strings.IndexByte(tok.name, '-'); idx > 0 {
				// "user-host" -> "user@host"
				newName = tok.name[:idx] + "@" + tok.name[idx+1:]
			} else if tok.name == "legacy" {
				newName = defaultUser + "@legacy"
			} else {
				safe := sanitizeName(tok.name)
				newName = defaultUser + "@" + safe
				log.Printf("token store migrate: renaming unrecognized token %q -> %q", tok.name, newName)
			}

			if _, err := s.pool.Exec(ctx,
				`UPDATE api_tokens SET name = $1, user_id = $2 WHERE id = $3`,
				newName, defaultUID, tok.id,
			); err != nil {
				return fmt.Errorf("token store migrate: updating token %d: %w", tok.id, err)
			}
		}
	}
	// Enforce NOT NULL + FK now that backfill is complete. On a fresh DB the
	// table is empty, so the constraints succeed trivially; adding NOT NULL
	// is a table scan, not a rewrite. Issue also requires a non-zero UserID,
	// so new rows can never be NULL.
	if _, err := s.pool.Exec(ctx, `
		ALTER TABLE api_tokens
			ALTER COLUMN user_id SET NOT NULL,
			ADD CONSTRAINT api_tokens_user_id_fkey
				FOREIGN KEY (user_id) REFERENCES memstore_users(id) ON DELETE RESTRICT`); err != nil {
		return fmt.Errorf("token store migrate: enforce user_id constraints: %w", err)
	}

	// Add index for user-scoped token lookups.
	if _, err := s.pool.Exec(ctx,
		`CREATE INDEX IF NOT EXISTS idx_api_tokens_user ON api_tokens (user_id) WHERE revoked_at IS NULL`,
	); err != nil {
		return fmt.Errorf("token store migrate: create user index: %w", err)
	}

	return nil
}

// Issue mints a new token, stores its hash, and returns the plaintext token.
// The plaintext is returned exactly once -- it cannot be retrieved later.
// Callers must surface it to the operator immediately and refuse to log it.
//
// Token names must match the <user>@<host> shape. opts.UserID is required.
func (s *TokenStore) Issue(ctx context.Context, name string, opts IssueOpts) (string, error) {
	if name == "" {
		return "", errors.New("token name is required")
	}
	if !tokenNameRe.MatchString(name) {
		return "", fmt.Errorf("token name %q must match <user>@<host> (e.g. matthew@laptop)", name)
	}
	if opts.UserID == 0 {
		return "", errors.New("token store issue: UserID is required")
	}
	token, err := generateToken()
	if err != nil {
		return "", err
	}
	hash := hashToken(token)

	var expiresAt *time.Time
	if opts.Expires > 0 {
		t := time.Now().Add(opts.Expires)
		expiresAt = &t
	}
	scopes := opts.Scopes
	if scopes == nil {
		scopes = []string{}
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO api_tokens (token_hash, name, user_id, scopes, expires_at)
		VALUES ($1, $2, $3, $4, $5)
	`, hash, name, opts.UserID, scopes, expiresAt)
	if err != nil {
		return "", fmt.Errorf("token store insert: %w", err)
	}
	return token, nil
}

// Verify resolves a presented bearer token to a name + scopes. Returns
// ErrTokenInvalid for any failure mode (not found, revoked, expired) so the
// caller can't accidentally distinguish.
//
// Side effect: bumps last_used_at on success. Failures do not write.
func (s *TokenStore) Verify(ctx context.Context, token string) (VerifyResult, error) {
	if token == "" {
		return VerifyResult{}, ErrTokenInvalid
	}
	hash := hashToken(token)

	var (
		id     int64
		name   string
		scopes []string
		userID int64
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, scopes, COALESCE(user_id, 0)
		  FROM api_tokens
		 WHERE token_hash = $1
		   AND revoked_at IS NULL
		   AND (expires_at IS NULL OR expires_at > now())
	`, hash).Scan(&id, &name, &scopes, &userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return VerifyResult{}, ErrTokenInvalid
	}
	if err != nil {
		return VerifyResult{}, fmt.Errorf("token verify: %w", err)
	}

	// Best-effort last_used_at update; failure here doesn't fail the request.
	_, _ = s.pool.Exec(ctx, `UPDATE api_tokens SET last_used_at = now() WHERE id = $1`, id)

	return VerifyResult{Name: name, Scopes: scopes, UserID: userID}, nil
}

// List returns metadata for all non-revoked tokens, ordered by creation time.
// The plaintext token is never returned (it's not stored).
func (s *TokenStore) List(ctx context.Context) ([]TokenInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, COALESCE(user_id, 0), scopes, created_at, last_used_at, expires_at, revoked_at
		  FROM api_tokens
		 WHERE revoked_at IS NULL
		 ORDER BY created_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TokenInfo
	for rows.Next() {
		var t TokenInfo
		if err := rows.Scan(&t.ID, &t.Name, &t.UserID, &t.Scopes, &t.CreatedAt, &t.LastUsedAt, &t.ExpiresAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Revoke marks all active tokens with the given name as revoked. Returns the
// number of tokens revoked.
func (s *TokenStore) Revoke(ctx context.Context, name string) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE api_tokens
		   SET revoked_at = now()
		 WHERE name = $1 AND revoked_at IS NULL
	`, name)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// RevokeByUser revokes every active token owned by userID and returns the
// number revoked. This is the mechanism behind 'memstore admin disable-user':
// a user with no active token cannot authenticate to the daemon.
func (s *TokenStore) RevokeByUser(ctx context.Context, userID int64) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE api_tokens
		   SET revoked_at = now()
		 WHERE user_id = $1 AND revoked_at IS NULL
	`, userID)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// Rotate issues a new token with the same name, scopes, and owner as the
// existing active token, then revokes the old one(s). Returns the new
// plaintext token. If multiple active tokens share the name, all are revoked
// and one new token is issued.
func (s *TokenStore) Rotate(ctx context.Context, name string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	var scopes []string
	var userID int64
	err = tx.QueryRow(ctx, `
		SELECT scopes, user_id FROM api_tokens
		 WHERE name = $1 AND revoked_at IS NULL
		 ORDER BY created_at DESC
		 LIMIT 1
	`, name).Scan(&scopes, &userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("no active token named %q", name)
	}
	if err != nil {
		return "", err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE api_tokens SET revoked_at = now()
		 WHERE name = $1 AND revoked_at IS NULL
	`, name); err != nil {
		return "", err
	}

	newToken, err := generateToken()
	if err != nil {
		return "", err
	}
	if scopes == nil {
		scopes = []string{}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO api_tokens (token_hash, name, user_id, scopes)
		VALUES ($1, $2, $3, $4)
	`, hashToken(newToken), name, userID, scopes); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return newToken, nil
}

// EnsureLegacyToken bootstraps the table with a row for the given plaintext
// key under name "legacy" if no row for that key already exists. Used by the
// daemon at startup to keep MEMSTORE_API_KEY-style deployments working
// without operator action. Returns true if a row was inserted.
func (s *TokenStore) EnsureLegacyToken(ctx context.Context, key string) (bool, error) {
	if key == "" {
		return false, nil
	}
	// api_tokens.user_id is NOT NULL: bind the legacy row to the default user.
	var uid int64
	err := s.pool.QueryRow(ctx, `
		SELECT u.id FROM memstore_users u
		 JOIN memstore_meta m ON m.key = 'default_user' AND m.value = u.name
		 LIMIT 1`).Scan(&uid)
	if err == pgx.ErrNoRows {
		return false, errors.New("token store: no default user recorded; run 'memstore admin tier3-init --default-user <name>' first")
	}
	if err != nil {
		return false, fmt.Errorf("token store: resolving default user: %w", err)
	}
	hash := hashToken(key)
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO api_tokens (token_hash, name, user_id, scopes)
		VALUES ($1, 'legacy', $2, '{"admin"}')
		ON CONFLICT (token_hash) DO NOTHING
	`, hash, uid)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// IsMemstoreToken reports whether the given string looks like a token issued
// by this package (i.e. carries the mst_ prefix). Useful when operators paste
// a legacy key and you want to warn them it's not in the new format.
func IsMemstoreToken(s string) bool {
	return strings.HasPrefix(s, tokenPrefix)
}

func generateToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("token store: random: %w", err)
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
