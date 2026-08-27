package memstore

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// Fact serializes with Go field names, which is the shape the HTTP API has
// always emitted: GET /v1/facts/{id} returns "ID", "Content", "Namespace".
// Only UserID ever carried a tag, and it spelled itself "user_id" -- so one
// key out of eighteen arrived in a different convention from its neighbours.
//
// This asserts the whole struct rather than that one field: a tag added later
// in the other style is the same defect again, and it would otherwise be
// noticed by whoever was parsing the output at the time.
func TestFactJSONKeysAreGoFieldNames(t *testing.T) {
	now := time.Now().UTC()
	id := int64(7)
	f := Fact{
		ID: 1, Namespace: "n", UserID: 2, Content: "c", Subject: "s",
		Category: "cat", Kind: "k", Subsystem: "sub",
		Metadata: json.RawMessage(`{"a":1}`), SupersededBy: &id, SupersededAt: &now,
		ConfirmedCount: 3, LastConfirmedAt: &now, UseCount: 4, LastUsedAt: &now,
		InjectCount: 5, LastInjectedAt: &now, Embedding: []float32{0.5}, CreatedAt: now,
	}

	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}

	typ := reflect.TypeOf(f)
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if _, ok := got[name]; !ok {
			t.Errorf("field %s does not serialize as %q; keys present: %v", name, name, keysOf(got))
		}
		delete(got, name)
	}
	for leftover := range got {
		t.Errorf("unexpected JSON key %q -- not a Fact field name", leftover)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
