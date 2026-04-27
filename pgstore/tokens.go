package pgstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
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
	Scopes     []string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
}

// IssueOpts configures a new token.
type IssueOpts struct {
	Scopes  []string      // optional, e.g. {"read","write","admin"}
	Expires time.Duration // 0 = no expiry
}

// VerifyResult is what Verify returns for a valid token: enough to populate
// httpapi.Identity (kept here as primitives so this package doesn't import
// httpapi).
type VerifyResult struct {
	Name   string
	Scopes []string
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

func (s *TokenStore) migrate(ctx context.Context) error {
	stmts := []string{
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
	for _, stmt := range stmts {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("token store migrate: %w\nstatement: %s", err, stmt)
		}
	}
	return nil
}

// Issue mints a new token, stores its hash, and returns the plaintext token.
// The plaintext is returned exactly once — it cannot be retrieved later.
// Callers must surface it to the operator immediately and refuse to log it.
func (s *TokenStore) Issue(ctx context.Context, name string, opts IssueOpts) (string, error) {
	if name == "" {
		return "", errors.New("token name is required")
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
		INSERT INTO api_tokens (token_hash, name, scopes, expires_at)
		VALUES ($1, $2, $3, $4)
	`, hash, name, scopes, expiresAt)
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
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, scopes
		  FROM api_tokens
		 WHERE token_hash = $1
		   AND revoked_at IS NULL
		   AND (expires_at IS NULL OR expires_at > now())
	`, hash).Scan(&id, &name, &scopes)
	if errors.Is(err, pgx.ErrNoRows) {
		return VerifyResult{}, ErrTokenInvalid
	}
	if err != nil {
		return VerifyResult{}, fmt.Errorf("token verify: %w", err)
	}

	// Best-effort last_used_at update; failure here doesn't fail the request.
	_, _ = s.pool.Exec(ctx, `UPDATE api_tokens SET last_used_at = now() WHERE id = $1`, id)

	return VerifyResult{Name: name, Scopes: scopes}, nil
}

// List returns metadata for all non-revoked tokens, ordered by creation time.
// The plaintext token is never returned (it's not stored).
func (s *TokenStore) List(ctx context.Context) ([]TokenInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, scopes, created_at, last_used_at, expires_at, revoked_at
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
		if err := rows.Scan(&t.ID, &t.Name, &t.Scopes, &t.CreatedAt, &t.LastUsedAt, &t.ExpiresAt, &t.RevokedAt); err != nil {
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

// Rotate issues a new token with the same name and scopes as the existing
// active token, then revokes the old one(s). Returns the new plaintext token.
// If multiple active tokens share the name, all are revoked and one new token
// is issued.
func (s *TokenStore) Rotate(ctx context.Context, name string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	var scopes []string
	err = tx.QueryRow(ctx, `
		SELECT scopes FROM api_tokens
		 WHERE name = $1 AND revoked_at IS NULL
		 ORDER BY created_at DESC
		 LIMIT 1
	`, name).Scan(&scopes)
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
		INSERT INTO api_tokens (token_hash, name, scopes)
		VALUES ($1, $2, $3)
	`, hashToken(newToken), name, scopes); err != nil {
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
	hash := hashToken(key)
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO api_tokens (token_hash, name, scopes)
		VALUES ($1, 'legacy', '{"admin"}')
		ON CONFLICT (token_hash) DO NOTHING
	`, hash)
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
