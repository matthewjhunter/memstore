package httpapi_test

import (
	"context"
	"net/http"
	"reflect"
	"strconv"
	"testing"

	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
	"github.com/matthewjhunter/memstore/internal/teststore"
)

// claimingSessionStore claims the way pgstore does: a ref is new once per
// session, whichever channel shows it.
type claimingSessionStore struct {
	mockSessionStore
	claimed  map[string]bool
	channels []string
}

func newClaimingSessionStore() *claimingSessionStore {
	return &claimingSessionStore{claimed: map[string]bool{}}
}

func (s *claimingSessionStore) ClaimInjections(_ context.Context, sessionID, channel, refType string, refIDs []string) ([]string, error) {
	s.channels = append(s.channels, channel)
	fresh := []string{}
	for _, id := range refIDs {
		k := sessionID + "\x00" + refType + "\x00" + id
		if !s.claimed[k] {
			s.claimed[k] = true
			fresh = append(fresh, id)
		}
	}
	return fresh, nil
}

func claimRefs(t *testing.T, h *httpapi.Handler, body map[string]any) (int, []string) {
	t.Helper()
	resp := doJSON(t, h, "POST", "/v1/context/injections/claim", body)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	var out struct {
		New []string `json:"new"`
	}
	decodeJSON(t, resp, &out)
	return resp.StatusCode, out.New
}

func claimBody(session, channel string, ids ...string) map[string]any {
	return map[string]any{"session_id": session, "channel": channel, "ref_type": memstore.RefTypeFact, "ref_ids": ids}
}

func TestClaimInjections(t *testing.T) {
	emb := &mockEmbedder{dim: 4}
	ss := newClaimingSessionStore()
	h := httpapi.New(teststore.New(t, emb, "test"), emb, "", httpapi.WithSessionStore(ss))

	code, got := claimRefs(t, h, claimBody("s", memstore.ChannelFileTrigger, "5", "6"))
	if code != http.StatusOK || !reflect.DeepEqual(got, []string{"5", "6"}) {
		t.Fatalf("first claim: %d %v, want 200 [5 6]", code, got)
	}
	code, got = claimRefs(t, h, claimBody("s", memstore.ChannelFileTrigger, "6", "7"))
	if code != http.StatusOK || !reflect.DeepEqual(got, []string{"7"}) {
		t.Errorf("second claim: %d %v, want 200 [7]", code, got)
	}
	code, got = claimRefs(t, h, claimBody("s", memstore.ChannelStartup, "6"))
	if code != http.StatusOK || got == nil || len(got) != 0 {
		t.Errorf("nothing new: %d %#v, want 200 and an empty list", code, got)
	}
	want := []string{memstore.ChannelFileTrigger, memstore.ChannelFileTrigger, memstore.ChannelStartup}
	if !reflect.DeepEqual(ss.channels, want) {
		t.Errorf("channels passed to the store = %v, want %v", ss.channels, want)
	}
}

func TestClaimInjectionsRejectsBadInput(t *testing.T) {
	emb := &mockEmbedder{dim: 4}
	h := httpapi.New(teststore.New(t, emb, "test"), emb, "", httpapi.WithSessionStore(newClaimingSessionStore()))
	tooMany := make([]string, 501)
	for i := range tooMany {
		tooMany[i] = strconv.Itoa(i + 1)
	}
	for name, body := range map[string]map[string]any{
		"no session":      claimBody("", memstore.ChannelStartup, "5"),
		"unknown channel": claimBody("s", "email", "5"),
		"no channel":      claimBody("s", "", "5"),
		"no ref type":     {"session_id": "s", "channel": memstore.ChannelStartup, "ref_ids": []string{"5"}},
		"empty ref id":    claimBody("s", memstore.ChannelStartup, ""),
		"too many ids":    claimBody("s", memstore.ChannelStartup, tooMany...),
	} {
		if code, _ := claimRefs(t, h, body); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, code)
		}
	}
}

func TestClaimInjectionsNeedsAClaimingStore(t *testing.T) {
	emb := &mockEmbedder{dim: 4}
	h := httpapi.New(teststore.New(t, emb, "test"), emb, "", httpapi.WithSessionStore(&mockSessionStore{}))
	if code, _ := claimRefs(t, h, claimBody("s", memstore.ChannelStartup, "5")); code != http.StatusNotImplemented {
		t.Errorf("status %d, want 501", code)
	}
}

// A fact the file trigger showed in full is not repeated by recall later in the
// session. A startup task shows only its title, so it does not hide the full
// text from recall.
func TestFileTriggerClaimIsNotRepeatedByRecall(t *testing.T) {
	ctx := context.Background()
	emb := &mockEmbedder{dim: 4}
	store := teststore.New(t, emb, "test")
	seedFacts(t, store)
	id, err := store.Insert(ctx, memstore.Fact{Content: "Quokka deploys run from the ansible directory", Subject: "quokka", Category: "project"})
	if err != nil {
		t.Fatal(err)
	}
	sc := httpapi.NewSessionContext()
	t.Cleanup(sc.Stop)
	h := httpapi.New(store, emb, "", httpapi.WithSessionContext(sc), httpapi.WithSessionStore(newClaimingSessionStore()))

	recalled := func(session string) bool {
		t.Helper()
		resp := doJSON(t, h, "POST", "/v1/recall", map[string]any{"prompt": "where do quokka deploys run", "session_id": session})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("recall: status %d", resp.StatusCode)
		}
		var r struct {
			Facts []struct {
				ID int64 `json:"id"`
			} `json:"facts"`
		}
		decodeJSON(t, resp, &r)
		for _, f := range r.Facts {
			if f.ID == id {
				return true
			}
		}
		return false
	}
	if !recalled("control") {
		t.Fatal("recall does not return the fact at all; the test is not exercising the path")
	}

	ref := strconv.FormatInt(id, 10)
	if code, _ := claimRefs(t, h, claimBody("trig", memstore.ChannelFileTrigger, ref)); code != http.StatusOK {
		t.Fatalf("file-trigger claim: status %d", code)
	}
	if recalled("trig") {
		t.Error("recall repeated a fact the file trigger already showed")
	}
	if code, _ := claimRefs(t, h, claimBody("start", memstore.ChannelStartup, ref)); code != http.StatusOK {
		t.Fatalf("startup claim: status %d", code)
	}
	if !recalled("start") {
		t.Error("a startup task title kept recall from showing the fact")
	}
}
