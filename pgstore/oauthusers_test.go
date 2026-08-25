package pgstore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/infodancer/oidclient"
	"github.com/infodancer/oidclient/rpuser"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/memstore/httpapi"
	"github.com/matthewjhunter/memstore/pgstore"
)

func newOAuthUsers(t *testing.T) *pgstore.OAuthUserStore {
	t.Helper()
	return pgstore.NewOAuthUserStore(newTestStore(t))
}

const oauthTestIssuer = "https://webauth.example.test/t/memstore"

func identity(subject, email string, verified bool) rpuser.Identity {
	return rpuser.Identity{
		Issuer:        oauthTestIssuer,
		Subject:       subject,
		DisplayName:   email,
		Email:         email,
		EmailVerified: verified,
	}
}

// A driver no-rows result must be reported as rpuser.ErrNotFound, because that
// is the only signal rpuser uses to decide between "create this user" and
// "abort, the lookup failed". Passing the raw error through would abort a first
// login; returning ErrNotFound for a genuine failure would insert a duplicate
// row on every request while the database was unhealthy.
func TestOAuthUserStoreLookupMissReportsNotFound(t *testing.T) {
	users := newOAuthUsers(t)

	_, err := users.LookupBySubject(context.Background(), oauthTestIssuer, "nobody")
	if !errors.Is(err, rpuser.ErrNotFound) {
		t.Errorf("error %v does not match rpuser.ErrNotFound", err)
	}
}

func TestOAuthUserStoreCreateThenLookup(t *testing.T) {
	users := newOAuthUsers(t)
	ctx := context.Background()

	created, err := users.Create(ctx, identity("sub-abc", "someone@example.test", true))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Key == "" {
		t.Error("Create returned an empty key")
	}
	// The numeric id travels in Host, which is what the API layer scopes the
	// store by; without it every OAuth caller would need the key re-parsed.
	if _, ok := created.Host.(int64); !ok {
		t.Errorf("Create returned Host %T, want int64", created.Host)
	}

	found, err := users.LookupBySubject(ctx, oauthTestIssuer, "sub-abc")
	if err != nil {
		t.Fatalf("LookupBySubject: %v", err)
	}
	if found.Key != created.Key {
		t.Errorf("looked up key %q, want %q", found.Key, created.Key)
	}
	if found.Email != "someone@example.test" || !found.EmailVerified {
		t.Errorf("looked up email %q verified=%v, want someone@example.test/true", found.Email, found.EmailVerified)
	}
}

// The same subject at a different issuer is a different person, and the lookup
// must not find one for the other.
func TestOAuthUserStoreScopesSubjectsToTheIssuer(t *testing.T) {
	users := newOAuthUsers(t)
	ctx := context.Background()

	a := identity("shared-subject", "a@example.test", true)
	b := a
	b.Issuer = "https://other.example.test/t/memstore"
	b.Email = "b@example.test"

	ua, err := users.Create(ctx, a)
	if err != nil {
		t.Fatalf("Create(a): %v", err)
	}
	ub, err := users.Create(ctx, b)
	if err != nil {
		t.Fatalf("Create(b): %v", err)
	}
	if ua.Key == ub.Key {
		t.Fatalf("one subject at two issuers produced a single user %q", ua.Key)
	}

	// Look up at BOTH issuers. Checking only one can pass by row-ordering luck
	// if the query ignores the issuer entirely -- whichever row came back first
	// might happen to be the expected one. Requiring each issuer to return its
	// own row makes that impossible.
	foundA, err := users.LookupBySubject(ctx, a.Issuer, "shared-subject")
	if err != nil {
		t.Fatalf("LookupBySubject(a): %v", err)
	}
	if foundA.Key != ua.Key {
		t.Errorf("lookup at issuer a returned %q, want %q", foundA.Key, ua.Key)
	}
	foundB, err := users.LookupBySubject(ctx, b.Issuer, "shared-subject")
	if err != nil {
		t.Fatalf("LookupBySubject(b): %v", err)
	}
	if foundB.Key != ub.Key {
		t.Errorf("lookup at issuer b returned %q, want %q", foundB.Key, ub.Key)
	}
}

// The uniqueness rule is the database's, not merely the code's. A racing
// double-create on a first login must fail the second insert rather than split
// one person across two rows holding half their memories each.
func TestOAuthUserStoreRejectsADuplicateSubject(t *testing.T) {
	users := newOAuthUsers(t)
	ctx := context.Background()

	if _, err := users.Create(ctx, identity("sub-dup", "one@example.test", true)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := users.Create(ctx, identity("sub-dup", "two@example.test", true)); err == nil {
		t.Error("a second row was created for an existing (issuer, subject)")
	}
}

// Two subjects sharing an email are two users. Nothing in the schema may allow
// an upstream that recycles an address to hand its next owner someone else's
// memories.
func TestOAuthUserStoreAllowsTwoSubjectsToShareAnEmail(t *testing.T) {
	users := newOAuthUsers(t)
	ctx := context.Background()
	shared := "recycled@example.test"

	first, err := users.Create(ctx, identity("sub-old", shared, true))
	if err != nil {
		t.Fatalf("Create(first): %v", err)
	}
	second, err := users.Create(ctx, identity("sub-new", shared, true))
	if err != nil {
		t.Fatalf("Create(second): %v -- a shared email must not collide", err)
	}
	if first.Key == second.Key {
		t.Errorf("two subjects sharing an email resolved to one user %q", first.Key)
	}
}

func TestOAuthUserStoreSyncEmail(t *testing.T) {
	users := newOAuthUsers(t)
	ctx := context.Background()

	created, err := users.Create(ctx, identity("sub-sync", "old@example.test", false))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := users.SyncEmail(ctx, created.Key, "new@example.test", true); err != nil {
		t.Fatalf("SyncEmail: %v", err)
	}

	found, err := users.LookupBySubject(ctx, oauthTestIssuer, "sub-sync")
	if err != nil {
		t.Fatalf("LookupBySubject: %v", err)
	}
	if found.Email != "new@example.test" || !found.EmailVerified {
		t.Errorf("after sync: email %q verified=%v, want new@example.test/true", found.Email, found.EmailVerified)
	}
	// Syncing must never make a second row.
	if found.Key != created.Key {
		t.Errorf("sync moved the user from %q to %q", created.Key, found.Key)
	}
}

// Locally administered users predate OAuth and carry no issuer or subject. The
// partial unique index must let any number of them coexist, or the migration
// breaks every existing deployment on upgrade.
func TestOAuthUserStoreToleratesLocalUsersWithoutSubjects(t *testing.T) {
	store := newTestStore(t)
	users := pgstore.NewOAuthUserStore(store)
	ctx := context.Background()

	// newTestStore already seeds one local user; add more directly, the way an
	// existing deployment's rows look: a name, and no OAuth identity at all.
	pool, err := pgxpool.New(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	defer pool.Close()
	for _, name := range []string{"local-a", "local-b"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO memstore_users (namespace, name) VALUES ('test', $1)`, name); err != nil {
			t.Fatalf("creating local user %q: %v", name, err)
		}
	}

	// And an OAuth user alongside them still works.
	if _, err := users.Create(ctx, identity("sub-mixed", "mixed@example.test", true)); err != nil {
		t.Fatalf("Create alongside local users: %v", err)
	}
}

// The end-to-end path: an access token in, a memstore user id out, through the
// shared provisioning policy and the real SQL. This is what the OAuth verifier
// calls on every authenticated request.
func TestProvisioningResolverOverTheRealStore(t *testing.T) {
	users := newOAuthUsers(t)
	resolver, err := httpapi.NewProvisioningResolver(users, oauthTestIssuer, nil)
	if err != nil {
		t.Fatalf("NewProvisioningResolver: %v", err)
	}
	ctx := context.Background()
	tok := &oidclient.AccessToken{
		Subject:       "sub-e2e",
		Email:         "e2e@example.test",
		EmailVerified: true,
		Name:          "End To End",
	}

	first, err := resolver.ResolveUser(ctx, tok)
	if err != nil {
		t.Fatalf("ResolveUser (first): %v", err)
	}
	if first == 0 {
		t.Fatal("ResolveUser returned user 0, which is the service-scope sentinel")
	}

	// A second call is the ordinary case -- every request after the first --
	// and must return the same user rather than provisioning again.
	second, err := resolver.ResolveUser(ctx, tok)
	if err != nil {
		t.Fatalf("ResolveUser (second): %v", err)
	}
	if first != second {
		t.Errorf("the same token resolved to users %d then %d", first, second)
	}

	// A different subject is a different user, even from the same issuer.
	other, err := resolver.ResolveUser(ctx, &oidclient.AccessToken{Subject: "sub-other"})
	if err != nil {
		t.Fatalf("ResolveUser (other): %v", err)
	}
	if other == first {
		t.Errorf("two subjects resolved to the same user %d", first)
	}

	// email_verified reached the row, which is the field a faked Claims struct
	// would have silently pinned to false.
	stored, err := users.LookupBySubject(ctx, oauthTestIssuer, "sub-e2e")
	if err != nil {
		t.Fatalf("LookupBySubject: %v", err)
	}
	if !stored.EmailVerified {
		t.Error("email_verified was not persisted")
	}
}
