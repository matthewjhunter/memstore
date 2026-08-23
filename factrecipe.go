package memstore

import (
	embedding "github.com/matthewjhunter/go-embedding"
)

// headerLayoutVersion identifies how a chunk's header is assembled.
// go-embedding sees the task and the chunk size but not which fields memstore
// puts in the header, so memstore contributes this. Bump it whenever the header
// changes, so stored vectors are recognised as stale.
//
// v2: moved from a hand-rolled "subject: content" single line to
// FormatRecordForTask's record layout ("subject: <v>", blank line, body), which
// is what SplitRecord renders.
const headerLayoutVersion = "subject-record-v2"

// EmbedRecipe identifies everything besides the model and vector dimension that
// determines a stored fact vector: the task prefix, the chunk size, and the
// header layout.
//
// It exists so a recipe change is *detected* rather than remembered. Every
// previous one -- chunking, then task prefixes -- needed a hand-written
// migration whose only content was "delete every vector so the backfill
// re-runs", written from whoever changed the recipe recalling that they had.
// That does not scale, and the symptom of forgetting is degraded ranking rather
// than an error.
//
// The embed ceiling is deliberately NOT folded in, even though a ceiling below
// the chunk target does lower the effective chunk size and so does change the
// vectors.
//
// The reason is when each is knowable. The ceiling arrives through
// SetEmbedCeiling after the store is constructed, so a recipe that depended on
// it could not be checked at store-open -- the moment that matters, before a
// stale vector is served. Computing it at open with the ceiling still unset and
// again later with it set would make the recipe differ between the two, and a
// deployment clamped below the target would clear its vectors on every single
// startup. A check that cries wolf is one that gets routed around.
//
// The gap this leaves is narrow and worth stating: two deployments of the same
// model that clamp to different ceilings below the chunk target are not
// distinguished. Both known deployments run budgets well above it, where the
// ceiling has no effect on chunk size at all.
//
// The *token* target is hashed rather than the byte budget it converts to. The
// byte figure moves as calibration observations accumulate, so hashing it would
// make the recipe drift during normal operation and clear the corpus on a
// restart.
func EmbedRecipe(model string) string {
	return embedding.RecipeFingerprint(
		model,
		embedding.TaskRetrievalDocument,
		embedding.SplitOptions{MaxTokens: ChunkTargetTokens},
		headerLayoutVersion,
	)
}
