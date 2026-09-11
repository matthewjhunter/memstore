package mcpserver

import (
	"maps"

	"github.com/matthewjhunter/memstore"
)

// persistentGuidance says when to mark a fact persistent. It is repeated in the
// memory_store and memory_update descriptions because a model reads the tool it
// is calling, not the other one.
const persistentGuidance = "set true only for a fact that must never age out even if nothing " +
	"touches it for months -- a durable fact about the user or their world that is rarely " +
	"relevant but costly to lose. Keep it rare: most facts stay because they get used, and " +
	"memstore admin flush removes old facts that are not. It exempts the fact from age-based " +
	"removal only; superseding and deleting work as usual. When you supersede a persistent " +
	"fact, set persistent on the replacement too if it should keep the mark."

// withPersistent returns m with the persistent mark added when set is true.
// It copies rather than writing into m, which belongs to the caller's input.
func withPersistent(m Metadata, set bool) Metadata {
	if !set {
		return m
	}
	out := make(Metadata, len(m)+1)
	maps.Copy(out, m)
	out[memstore.MetaPersistent] = true
	return out
}
