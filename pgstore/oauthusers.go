package pgstore

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/infodancer/oidclient/rpuser"
	"github.com/jackc/pgx/v5"
)

// OAuth user provisioning.
//
// memstore_users is keyed (namespace, name) for locally administered users.
// An OAuth caller has no name to be keyed by -- it has an (issuer, subject)
// pair from the authorization server -- so this adds those as columns and a
// second uniqueness rule over them.
//
// The provisioning POLICY lives in github.com/infodancer/oidclient/rpuser,
// which memstore shares with the websites: users are keyed strictly on
// (issuer, subject), accounts are never matched or merged by email, and
// mutable claims are synced on the existing row rather than producing a second
// one. This file is only the SQL underneath it.
//
// Why never by email, since it is the obvious convenience: an upstream that
// recycles or reassigns an address would then hand its next owner someone
// else's memories. The subject is the only identifier the issuer promises not
// to reassign.

// migrateV11 adds the OAuth identity columns to memstore_users.
//
// The columns are nullable and the unique index is partial, so locally
// administered users -- every row that exists today -- keep working untouched
// with no issuer and no subject. Only OAuth-provisioned rows carry them.
func (s *PostgresStore) migrateV11(ctx context.Context) error {
	stmts := []string{
		`ALTER TABLE memstore_users ADD COLUMN IF NOT EXISTS oauth_issuer TEXT`,
		`ALTER TABLE memstore_users ADD COLUMN IF NOT EXISTS oauth_subject TEXT`,
		`ALTER TABLE memstore_users ADD COLUMN IF NOT EXISTS email TEXT`,
		`ALTER TABLE memstore_users ADD COLUMN IF NOT EXISTS email_verified BOOLEAN NOT NULL DEFAULT FALSE`,
		// The database enforces the key, not just the code above it. A racing
		// double-create on a first login must fail the second insert rather
		// than split one person across two rows -- and the constraint is also
		// what makes the never-match-by-email rule unviolatable from here.
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_memstore_users_oauth
		   ON memstore_users (namespace, oauth_issuer, oauth_subject)
		   WHERE oauth_subject IS NOT NULL`,
	}
	for _, q := range stmts {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("pgstore: migrateV11: %w", err)
		}
	}
	return nil
}

// OAuthUserStore implements rpuser.UserStore over memstore_users, scoped to one
// namespace. Construct it with NewOAuthUserStore.
type OAuthUserStore struct {
	store *PostgresStore
}

// NewOAuthUserStore returns a user store backed by s, for the namespace s was
// constructed with.
func NewOAuthUserStore(s *PostgresStore) *OAuthUserStore {
	return &OAuthUserStore{store: s}
}

var _ rpuser.UserStore = (*OAuthUserStore)(nil)

// LookupBySubject returns the user for (issuer, subject), or rpuser.ErrNotFound.
// It runs on every authenticated request and never writes.
func (u *OAuthUserStore) LookupBySubject(ctx context.Context, issuer, subject string) (rpuser.User, error) {
	var (
		id       int64
		email    *string
		verified bool
	)
	err := u.store.pool.QueryRow(ctx, `
		SELECT id, email, email_verified
		FROM memstore_users
		WHERE namespace = $1 AND oauth_issuer = $2 AND oauth_subject = $3`,
		u.store.namespace, issuer, subject,
	).Scan(&id, &email, &verified)
	if errors.Is(err, pgx.ErrNoRows) {
		// rpuser distinguishes "no such user" from "the lookup failed", and
		// creates only on the former -- so a driver no-rows result must be
		// mapped, never passed through. Getting this wrong would insert a
		// duplicate row on every request while the database was unhealthy.
		return rpuser.User{}, rpuser.ErrNotFound
	}
	if err != nil {
		return rpuser.User{}, fmt.Errorf("pgstore: looking up oauth user: %w", err)
	}
	return u.user(id, email, verified), nil
}

// SyncEmail persists changed mutable claims on an existing row.
func (u *OAuthUserStore) SyncEmail(ctx context.Context, userKey, email string, verified bool) error {
	id, err := parseUserKey(userKey)
	if err != nil {
		return err
	}
	if _, err := u.store.pool.Exec(ctx, `
		UPDATE memstore_users SET email = $1, email_verified = $2
		WHERE id = $3 AND namespace = $4`,
		email, verified, id, u.store.namespace,
	); err != nil {
		return fmt.Errorf("pgstore: syncing oauth user email: %w", err)
	}
	return nil
}

// localName is the value written to memstore_users.name for an OAuth user.
//
// name is the table's local identifier: unique per namespace under a constraint
// that predates OAuth, and referenced by memstore_meta's default_user. So it
// must be stable and collision free, which rules out the display name and the
// email address.
//
// The subject alone is not enough either. A subject string is only unique
// WITHIN its issuer -- "1" or "admin" is a plausible subject at two providers --
// and the pre-existing UNIQUE (namespace, name) would reject the second such
// user even though (issuer, subject) makes them distinct people. Including the
// issuer keeps the local name as unique as the pair it stands for.
//
// The result is verbose rather than hashed on purpose: it appears in admin
// listings, and an operator reading one should be able to see which provider a
// user arrived from without joining another column.
func localName(issuer, subject string) string {
	return "oauth:" + issuer + "|" + subject
}

// Create inserts a row for a first login.
func (u *OAuthUserStore) Create(ctx context.Context, id rpuser.Identity) (rpuser.User, error) {
	var newID int64
	err := u.store.pool.QueryRow(ctx, `
		INSERT INTO memstore_users (namespace, name, oauth_issuer, oauth_subject, email, email_verified)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id`,
		u.store.namespace, localName(id.Issuer, id.Subject), id.Issuer, id.Subject,
		nullIfEmpty(id.Email), id.EmailVerified,
	).Scan(&newID)
	if err != nil {
		return rpuser.User{}, fmt.Errorf("pgstore: creating oauth user: %w", err)
	}
	email := id.Email
	return u.user(newID, &email, id.EmailVerified), nil
}

// user builds the rpuser view of a row. The numeric id travels in Host as well
// as in Key: Host is the documented slot for the host's own representation, and
// it saves the caller re-parsing the key on every request.
func (u *OAuthUserStore) user(id int64, email *string, verified bool) rpuser.User {
	var e string
	if email != nil {
		e = *email
	}
	return rpuser.User{
		Key:           strconv.FormatInt(id, 10),
		Email:         e,
		EmailVerified: verified,
		Host:          id,
	}
}

func parseUserKey(key string) (int64, error) {
	id, err := strconv.ParseInt(key, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("pgstore: malformed user key %q: %w", key, err)
	}
	return id, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
