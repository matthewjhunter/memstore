package memstore

import "slices"

// What keeps a fact from aging out. memstore admin flush (#157) removes old,
// unused facts, and nothing about a fact's kind, category or subject exempts
// it: policy lives in the data, set by the user, not in lists in the code.

// MetaPersistent is the metadata key that exempts a fact from age-based
// removal. It is set by hand -- memory_store and memory_update take a
// persistent parameter -- and should be rare. It does not protect a fact from
// supersession or deletion.
//
// It is a metadata key rather than a column, so it travels through every path
// metadata already does: the HTTP API, export and import, flush backups.
const MetaPersistent = "persistent"

// ClosedTaskStatuses returns the statuses that end a task. A task with any
// other status, or none, is open work, and flush never removes open work.
func ClosedTaskStatuses() []string { return slices.Clone(closedTaskStatuses) }
