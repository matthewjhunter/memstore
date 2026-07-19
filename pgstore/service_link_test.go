package pgstore_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/pgstore"
)

// Service-scope LinkFacts derives the link's owner from its endpoints. Before
// this was defined, the service-scope branch stamped user_id = 0, which
// violates the FK to memstore_users -- dead code that failed closed, but
// inconsistent with ownerFor's treatment of Insert.
func TestLinkFacts_ServiceScope(t *testing.T) {
	ctx := context.Background()
	base := newTestStore(t)

	pool, err := pgxpool.New(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	uidA, err := pgstore.EnsureUser(ctx, pool, "test", "svc-link-a")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	uidB, err := pgstore.EnsureUser(ctx, pool, "test", "svc-link-b")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	a, err := base.ForUser(uidA)
	if err != nil {
		t.Fatalf("ForUser(%d): %v", uidA, err)
	}
	b, err := base.ForUser(uidB)
	if err != nil {
		t.Fatalf("ForUser(%d): %v", uidB, err)
	}

	fa1 := mustInsert(t, a, "fact a1", "svc-link")
	fa2 := mustInsert(t, a, "fact a2", "svc-link")
	fb := mustInsert(t, b, "fact b1", "svc-link")

	svc := base.ServiceScope()

	t.Run("same-owner endpoints: link created and stamped with that owner", func(t *testing.T) {
		linkID, err := svc.LinkFacts(ctx, fa1, fa2, "related", false, "", nil)
		if err != nil {
			t.Fatalf("service-scope LinkFacts: %v", err)
		}
		var owner int64
		if err := pool.QueryRow(ctx,
			`SELECT user_id FROM memstore_links WHERE id = $1`, linkID,
		).Scan(&owner); err != nil {
			t.Fatalf("reading link owner: %v", err)
		}
		if owner != uidA {
			t.Errorf("link user_id = %d, want %d (the endpoints' owner)", owner, uidA)
		}
		// The owner sees the link through their own scope.
		l, err := a.GetLink(ctx, linkID)
		if err != nil || l == nil {
			t.Errorf("owner cannot see service-created link: link=%v err=%v", l, err)
		}
	})

	t.Run("cross-user endpoints: rejected, nothing inserted", func(t *testing.T) {
		var before int64
		pool.QueryRow(ctx, `SELECT COUNT(*) FROM memstore_links`).Scan(&before)

		if _, err := svc.LinkFacts(ctx, fa1, fb, "related", false, "", nil); err == nil {
			t.Fatal("service-scope link across users succeeded, want error")
		}

		var after int64
		pool.QueryRow(ctx, `SELECT COUNT(*) FROM memstore_links`).Scan(&after)
		if after != before {
			t.Errorf("link count changed %d -> %d on rejected cross-user link", before, after)
		}
	})

	t.Run("self-link under service scope", func(t *testing.T) {
		if _, err := svc.LinkFacts(ctx, fa1, fa1, "self", false, "", nil); err != nil {
			t.Errorf("service-scope self-link: %v", err)
		}
	})
}

func mustInsert(t *testing.T, s memstore.Store, content, subject string) int64 {
	t.Helper()
	id, err := s.Insert(context.Background(), memstore.Fact{Content: content, Subject: subject})
	if err != nil {
		t.Fatalf("insert %q: %v", content, err)
	}
	return id
}
