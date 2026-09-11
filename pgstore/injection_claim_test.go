package pgstore_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/pgstore"
)

// A claim records the refs a channel showed and returns the ones new to the
// session, in the order given, each once.
func TestClaimInjections(t *testing.T) {
	ss, pool := newTestSessionStore(t)
	ctx := context.Background()
	claim := func(session string, ids ...string) []string {
		t.Helper()
		got, err := ss.ClaimInjections(ctx, session, memstore.ChannelFileTrigger, memstore.RefTypeFact, ids)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	if got := claim("s1", "5", "6", "5"); !reflect.DeepEqual(got, []string{"5", "6"}) {
		t.Errorf("first claim = %v, want [5 6]", got)
	}
	if got := claim("s1", "6", "7"); !reflect.DeepEqual(got, []string{"7"}) {
		t.Errorf("second claim = %v, want [7]", got)
	}
	if got := claim("s2", "5"); !reflect.DeepEqual(got, []string{"5"}) {
		t.Errorf("another session = %v, want [5]", got)
	}
	if got := claim("s1"); len(got) != 0 {
		t.Errorf("empty claim = %v", got)
	}

	var channel string
	var rank int
	if err := pool.QueryRow(ctx, `SELECT channel, rank FROM context_injections WHERE session_id = 's1' AND ref_id = '7'`).Scan(&channel, &rank); err != nil {
		t.Fatal(err)
	}
	if channel != memstore.ChannelFileTrigger || rank != 1 {
		t.Errorf("ref 7 recorded as channel %q rank %d, want %q rank 1", channel, rank, memstore.ChannelFileTrigger)
	}
}

// The first channel to show a ref keeps it: a fact recall already injected is
// not new to the file-trigger channel and stays recorded as recall's.
func TestClaimInjectionsAfterRecall(t *testing.T) {
	ss, pool := newTestSessionStore(t)
	ctx := context.Background()
	if err := ss.RecordInjection(ctx, "s1", "5", memstore.RefTypeFact, 0); err != nil {
		t.Fatal(err)
	}
	if err := ss.RecordInjection(ctx, "s1", "3", memstore.RefTypeHint, 0); err != nil {
		t.Fatal(err)
	}
	got, err := ss.ClaimInjections(ctx, "s1", memstore.ChannelFileTrigger, memstore.RefTypeFact, []string{"5", "6"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"6"}) {
		t.Errorf("claim = %v, want [6]", got)
	}

	want := map[string]string{"5": memstore.ChannelRecall, "3": memstore.ChannelHint, "6": memstore.ChannelFileTrigger}
	if got := injectionChannels(t, pool, "s1"); !reflect.DeepEqual(got, want) {
		t.Errorf("channels = %v, want %v", got, want)
	}
}

// Rows from before the channel column get one from their ref type when the
// store migrates: recall wrote the fact rows and the prompt hook the hints.
func TestInjectionChannelBackfill(t *testing.T) {
	ss, pool := newTestSessionStore(t)
	ctx := context.Background()
	if err := ss.RecordInjection(ctx, "s1", "5", memstore.RefTypeFact, 0); err != nil {
		t.Fatal(err)
	}
	if err := ss.RecordInjection(ctx, "s1", "3", memstore.RefTypeHint, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE context_injections SET channel = ''`); err != nil {
		t.Fatal(err)
	}
	if _, err := pgstore.NewSessionStore(ctx, pool); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"5": memstore.ChannelRecall, "3": memstore.ChannelHint}
	if got := injectionChannels(t, pool, "s1"); !reflect.DeepEqual(got, want) {
		t.Errorf("channels after migrate = %v, want %v", got, want)
	}
}

// injectionChannels maps each ref recorded for the session to its channel.
func injectionChannels(t *testing.T, pool *pgxpool.Pool, session string) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT ref_id, channel FROM context_injections WHERE session_id = $1`, session)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	channels := map[string]string{}
	for rows.Next() {
		var ref, ch string
		if err := rows.Scan(&ref, &ch); err != nil {
			t.Fatal(err)
		}
		channels[ref] = ch
	}
	return channels
}

// The auto-rater reads recall's injections only. Exposure logged on the other
// channels is for measurement and must not change what gets rated.
func TestRaterReadsRecallInjectionsOnly(t *testing.T) {
	ss, _ := newTestSessionStore(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ss.RecordInjection(ctx, "s1", "5", memstore.RefTypeFact, 0))
	_, err := ss.ClaimInjections(ctx, "s1", memstore.ChannelFileTrigger, memstore.RefTypeFact, []string{"6"})
	must(err)
	_, err = ss.ClaimInjections(ctx, "s1", memstore.ChannelStartup, memstore.RefTypeFact, []string{"7"})
	must(err)
	_, err = ss.ClaimInjections(ctx, "s2", memstore.ChannelFileTrigger, memstore.RefTypeFact, []string{"8"})
	must(err)
	for _, sid := range []string{"s1", "s2"} {
		must(ss.SaveTurns(ctx, sid, []memstore.SessionTurn{{SessionID: sid, UUID: "a", Role: "assistant", Content: "done"}}))
	}

	ids, err := ss.GetInjectedFactIDs(ctx, "s1")
	must(err)
	if !reflect.DeepEqual(ids, []int64{5}) {
		t.Errorf("GetInjectedFactIDs = %v, want [5]", ids)
	}
	sessions, err := ss.UnratedFactSessions(ctx)
	must(err)
	if !reflect.DeepEqual(sessions, []string{"s1"}) {
		t.Errorf("UnratedFactSessions = %v, want [s1]", sessions)
	}
}
