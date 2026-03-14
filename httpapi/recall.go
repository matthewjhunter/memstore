package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/matthewjhunter/memstore"
)

// recallRequest is the input for POST /v1/recall.
type recallRequest struct {
	Prompt    string `json:"prompt"`
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	Limit     int    `json:"limit"`  // max facts (default 5)
	Budget    int    `json:"budget"` // max chars for formatted output (default 2000)
}

// recallResponse is the output of POST /v1/recall.
type recallResponse struct {
	Context  string       `json:"context"`  // pre-formatted text block for hook injection
	Facts    []recallFact `json:"facts"`    // structured results
	Keywords []string     `json:"keywords"` // IDF-extracted keywords used for search
}

type recallFact struct {
	ID       int64   `json:"id"`
	Subject  string  `json:"subject"`
	Category string  `json:"category"`
	Content  string  `json:"content"`
	Score    float64 `json:"score"`
}

// recallDefaults
const (
	defaultRecallLimit  = 5
	defaultRecallBudget = 2000
	maxRecallLimit      = 20
	maxFactChars        = 300
	maxKeywords         = 5
	minIDFFloor         = 0.5  // absolute minimum IDF threshold
	minIDFFraction      = 0.15 // fraction of log(N) used as IDF threshold
	minScoreRatio       = 0.3  // facts scoring below 30% of the top fact are dropped
)

// stopWords are filtered from keyword extraction. Kept small — IDF scoring
// handles most frequency-based filtering, this just removes the obvious noise.
var stopWords = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true, "but": true,
	"in": true, "on": true, "at": true, "to": true, "for": true, "of": true,
	"with": true, "is": true, "it": true, "be": true, "are": true, "was": true,
	"were": true, "been": true, "have": true, "has": true, "had": true,
	"do": true, "does": true, "did": true, "will": true, "would": true,
	"could": true, "should": true, "may": true, "might": true,
	"i": true, "me": true, "my": true, "you": true, "your": true, "we": true,
	"our": true, "they": true, "their": true, "he": true, "she": true,
	"this": true, "that": true, "what": true, "how": true, "when": true,
	"where": true, "why": true, "who": true, "not": true, "no": true,
	"from": true, "by": true, "as": true, "if": true, "its": true,
	"about": true, "into": true, "just": true, "can": true, "also": true,
}

func (h *Handler) handleContextTouch(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SessionID string   `json:"session_id"`
		Files     []string `json:"files"`
	}
	if !readJSON(r, w, &input) {
		return
	}
	if input.SessionID == "" {
		writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	if h.sessionCtx != nil {
		h.sessionCtx.TouchFiles(input.SessionID, input.Files)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) handleRecall(w http.ResponseWriter, r *http.Request) {
	var req recallRequest
	if !readJSON(r, w, &req) {
		return
	}
	if req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	if req.Limit <= 0 {
		req.Limit = defaultRecallLimit
	}
	if req.Limit > maxRecallLimit {
		req.Limit = maxRecallLimit
	}
	if req.Budget <= 0 {
		req.Budget = defaultRecallBudget
	}

	resp, err := h.recall(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) recall(ctx context.Context, req recallRequest) (*recallResponse, error) {
	// Extract candidate words from prompt.
	words := extractCandidateWords(req.Prompt)
	if len(words) == 0 {
		return &recallResponse{}, nil
	}

	// Score by IDF if the store supports it.
	keywords := scoreAndSelectKeywords(ctx, h.store, words)
	if len(keywords) == 0 {
		return &recallResponse{}, nil
	}

	// Derive project context from CWD.
	project := ""
	if req.CWD != "" {
		project = filepath.Base(req.CWD)
	}

	// Get recent files for context boosting.
	var recentFiles []string
	if h.sessionCtx != nil && req.SessionID != "" {
		recentFiles = h.sessionCtx.RecentFiles(req.SessionID)
	}

	// Search: one FTS query per keyword, merge results.
	seen := make(map[int64]*scoredFact)
	for _, kw := range keywords {
		opts := memstore.SearchOpts{
			MaxResults: req.Limit * 2, // overfetch for re-ranking
			OnlyActive: true,
		}
		results, err := h.store.SearchFTS(ctx, kw, opts)
		if err != nil {
			continue // single keyword failure is not fatal
		}
		for _, r := range results {
			if existing, ok := seen[r.Fact.ID]; ok {
				// Boost facts matching multiple keywords.
				existing.score += r.Combined
				existing.keywordHits++
			} else {
				seen[r.Fact.ID] = &scoredFact{
					fact:        r.Fact,
					score:       r.Combined,
					keywordHits: 1,
				}
			}
		}
	}

	// Evaluate CWD-pattern triggers and merge their loaded facts.
	if req.CWD != "" {
		cwdFacts := h.evalCWDTriggers(ctx, req.CWD)
		for _, f := range cwdFacts {
			if _, ok := seen[f.ID]; !ok {
				seen[f.ID] = &scoredFact{
					fact:        f,
					score:       2.0, // high base score so triggered facts surface
					keywordHits: 0,
				}
			}
		}
	}

	// Fetch historical feedback scores for candidates.
	var feedbackScores map[string]float64
	if scorer, ok := h.sessionStore.(memstore.FeedbackScorer); ok && len(seen) > 0 {
		refIDs := make([]string, 0, len(seen))
		for id := range seen {
			refIDs = append(refIDs, strconv.FormatInt(id, 10))
		}
		if scores, err := scorer.FeedbackScores(ctx, refIDs, memstore.RefTypeFact); err == nil {
			feedbackScores = scores
		}
	}

	// Apply context boosts and filtering.
	var candidates []scoredFact
	for _, sf := range seen {
		// Skip draft/learned facts.
		if isDraft(sf.fact) {
			continue
		}
		// Skip session-activity facts.
		if sf.fact.Subject == "session-activity" {
			continue
		}
		// Skip trigger facts (operational metadata, not useful content).
		if sf.fact.Kind == "trigger" {
			continue
		}

		sym := isSymbol(sf.fact)

		// Boost high-value kinds (human-stored decisions and conventions).
		switch sf.fact.Kind {
		case "decision":
			sf.score *= 1.5
		case "convention", "invariant":
			sf.score *= 1.3
		}

		// Demote symbol/code-doc facts — useful when editing that file,
		// noise in general recall.
		if sym {
			sf.score *= 0.2
		}

		// Boost for project match, demote unrelated facts.
		if project != "" {
			if subjectMatchesProject(sf.fact.Subject, project) {
				if !sym {
					sf.score *= 2.5
				}
				// Symbol facts from the current project get no project
				// boost — the base symbol demotion keeps them low.
			} else if sf.fact.Category != "project" && sf.fact.Category != "preference" {
				// Non-project, non-preference facts from other subjects are likely noise.
				sf.score *= 0.3
			}
		}

		// Boost for file context match.
		if len(recentFiles) > 0 {
			if matchesFileContext(sf.fact, recentFiles) {
				sf.score *= 1.3
			}
		}

		// Boost for multiple keyword hits.
		if sf.keywordHits > 1 {
			sf.score *= 1.0 + 0.2*float64(sf.keywordHits-1)
		}

		// Apply feedback boost: consistently useful facts get up to 1.3x,
		// consistently not useful get down to 0.7x, no feedback = no effect.
		if avg, ok := feedbackScores[strconv.FormatInt(sf.fact.ID, 10)]; ok {
			sf.score *= 1.0 + 0.3*avg
		}

		candidates = append(candidates, *sf)
	}

	// Sort by score descending.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	// Filter out facts already returned in this session.
	if h.sessionCtx != nil && req.SessionID != "" {
		var allIDs []int64
		for _, c := range candidates {
			allIDs = append(allIDs, c.fact.ID)
		}
		unseenSet := make(map[int64]bool)
		for _, id := range h.sessionCtx.FilterSeen(req.SessionID, allIDs) {
			unseenSet[id] = true
		}
		var filtered []scoredFact
		for _, c := range candidates {
			if unseenSet[c.fact.ID] {
				filtered = append(filtered, c)
			}
		}
		candidates = filtered
	}

	// Compute minimum score threshold relative to the top result.
	var minScore float64
	if len(candidates) > 0 {
		minScore = candidates[0].score * minScoreRatio
	}

	// Enforce relevance threshold, limit, and budget.
	var facts []recallFact
	totalChars := 0
	for _, c := range candidates {
		if c.score < minScore {
			break // sorted descending, rest will also be below threshold
		}
		if len(facts) >= req.Limit {
			break
		}
		if totalChars >= req.Budget {
			break
		}

		content := c.fact.Content
		if len(content) > maxFactChars {
			content = content[:maxFactChars] + "..."
		}

		remaining := req.Budget - totalChars
		block := formatFactBlock(c.fact, content)
		if len(block) > remaining {
			break
		}

		facts = append(facts, recallFact{
			ID:       c.fact.ID,
			Subject:  c.fact.Subject,
			Category: c.fact.Category,
			Content:  content,
			Score:    c.score,
		})
		totalChars += len(block)
	}

	// Record returned facts so they won't be injected again this session.
	if h.sessionCtx != nil && req.SessionID != "" && len(facts) > 0 {
		seenIDs := make([]int64, len(facts))
		for i, f := range facts {
			seenIDs[i] = f.ID
		}
		h.sessionCtx.MarkSeen(req.SessionID, seenIDs)
	}

	// Record fact injections server-side for feedback tracking.
	if h.sessionStore != nil && req.SessionID != "" && len(facts) > 0 {
		for rank, f := range facts {
			h.sessionStore.RecordInjection(ctx, req.SessionID, strconv.FormatInt(f.ID, 10), memstore.RefTypeFact, rank)
		}
	}

	// Format the context block.
	contextBlock := formatRecallContext(facts)

	return &recallResponse{
		Context:  contextBlock,
		Facts:    facts,
		Keywords: keywords,
	}, nil
}

type scoredFact struct {
	fact        memstore.Fact
	score       float64
	keywordHits int
}

// extractCandidateWords splits the prompt into lowercase words, removes
// stop words and short words, and returns unique candidates.
func extractCandidateWords(prompt string) []string {
	seen := make(map[string]bool)
	var words []string
	for _, w := range strings.Fields(prompt) {
		w = strings.ToLower(strings.Trim(w, ".,;:!?\"'()[]{}"))
		if len(w) < 3 || stopWords[w] || seen[w] {
			continue
		}
		seen[w] = true
		words = append(words, w)
	}
	return words
}

// scoreAndSelectKeywords uses IDF scoring if the store supports TermDocCounts,
// otherwise falls back to selecting the longest words (crude distinctiveness proxy).
func scoreAndSelectKeywords(ctx context.Context, store memstore.Store, words []string) []string {
	if len(words) == 0 {
		return nil
	}

	type wordScore struct {
		word  string
		score float64
	}

	tc, ok := store.(memstore.TermCounter)
	if ok {
		counts, totalDocs, err := tc.TermDocCounts(ctx, words)
		if err == nil && totalDocs > 0 {
			var scored []wordScore
			for _, w := range words {
				df := counts[w] // 0 if term not in index
				// IDF = log(N / (df + 1)) — +1 to avoid division by zero.
				idf := math.Log(float64(totalDocs) / float64(df+1))
				scored = append(scored, wordScore{word: w, score: idf})
			}
			sort.Slice(scored, func(i, j int) bool {
				return scored[i].score > scored[j].score
			})
			// Dynamic IDF threshold: scales with corpus size so small test
			// corpora still work, but large corpora filter out common words.
			threshold := math.Log(float64(totalDocs)) * minIDFFraction
			if threshold < minIDFFloor {
				threshold = minIDFFloor
			}
			var result []string
			for _, ws := range scored {
				if ws.score < threshold {
					break // sorted descending, rest will also be below threshold
				}
				result = append(result, ws.word)
				if len(result) >= maxKeywords {
					break
				}
			}
			return result
		}
	}

	// Fallback: longest words first (rarer words tend to be longer).
	sort.Slice(words, func(i, j int) bool {
		return len(words[i]) > len(words[j])
	})
	if len(words) > maxKeywords {
		words = words[:maxKeywords]
	}
	return words
}

// isSymbol returns true if the fact is a symbol/code-doc fact (learned from code,
// useful when editing that file but noise in general recall).
func isSymbol(f memstore.Fact) bool {
	// Subject-based detection for file/symbol facts.
	if strings.HasPrefix(f.Subject, "file:") || strings.HasPrefix(f.Subject, "sym:") {
		return true
	}
	// Metadata-based detection.
	if len(f.Metadata) == 0 {
		return false
	}
	var meta map[string]any
	if err := json.Unmarshal(f.Metadata, &meta); err != nil {
		return false
	}
	surface, _ := meta["surface"].(string)
	return surface == "symbol"
}

// subjectMatchesProject checks if a fact's subject matches the CWD-derived project name.
// Handles org/repo subjects like "infodancer/oidclient" matching project "oidclient".
func subjectMatchesProject(subject, project string) bool {
	if strings.EqualFold(subject, project) {
		return true
	}
	if i := strings.LastIndex(subject, "/"); i >= 0 {
		return strings.EqualFold(subject[i+1:], project)
	}
	return false
}

// isDraft returns true if the fact has quality metadata indicating a draft/learned fact.
func isDraft(f memstore.Fact) bool {
	if len(f.Metadata) == 0 {
		return false
	}
	var meta map[string]any
	if err := json.Unmarshal(f.Metadata, &meta); err != nil {
		return false
	}
	q, _ := meta["quality"].(string)
	return strings.HasPrefix(q, "local:")
}

// matchesFileContext checks if a fact's content or subject references any of the recent files.
func matchesFileContext(f memstore.Fact, recentFiles []string) bool {
	lower := strings.ToLower(f.Content + " " + f.Subject)
	for _, file := range recentFiles {
		base := strings.ToLower(filepath.Base(file))
		if strings.Contains(lower, base) {
			return true
		}
	}
	return false
}

// evalCWDTriggers finds kind=trigger facts with signal_type=cwd_pattern,
// matches them against the CWD, and loads the referenced context facts.
func (h *Handler) evalCWDTriggers(ctx context.Context, cwd string) []memstore.Fact {
	triggers, err := h.store.List(ctx, memstore.QueryOpts{
		Kind:       "trigger",
		OnlyActive: true,
		MetadataFilters: []memstore.MetadataFilter{
			{Key: "signal_type", Op: "=", Value: "cwd_pattern"},
		},
	})
	if err != nil || len(triggers) == 0 {
		return nil
	}

	seen := make(map[int64]bool)
	var result []memstore.Fact

	for _, t := range triggers {
		var meta map[string]any
		if len(t.Metadata) == 0 {
			continue
		}
		if err := json.Unmarshal(t.Metadata, &meta); err != nil {
			continue
		}
		signal, _ := meta["signal"].(string)
		if signal == "" || !memstore.MatchFilePattern(signal, cwd) {
			continue
		}

		loadSub, _ := meta["load_subsystem"].(string)
		loadSubject, _ := meta["load_subject"].(string)
		if loadSub == "" && loadSubject == "" {
			continue
		}

		opts := memstore.QueryOpts{
			Subsystem:  loadSub,
			Subject:    loadSubject,
			OnlyActive: true,
		}

		// If load_kinds specified, query each kind.
		var kinds []string
		if rawKinds, ok := meta["load_kinds"].([]any); ok {
			for _, k := range rawKinds {
				if s, ok := k.(string); ok {
					kinds = append(kinds, s)
				}
			}
		}

		if len(kinds) > 0 {
			for _, kind := range kinds {
				opts.Kind = kind
				facts, err := h.store.List(ctx, opts)
				if err != nil {
					continue
				}
				for _, f := range facts {
					if !seen[f.ID] {
						seen[f.ID] = true
						result = append(result, f)
					}
				}
			}
		} else {
			facts, err := h.store.List(ctx, opts)
			if err != nil {
				continue
			}
			for _, f := range facts {
				if !seen[f.ID] {
					seen[f.ID] = true
					result = append(result, f)
				}
			}
		}
	}

	return result
}

func formatFactBlock(f memstore.Fact, content string) string {
	return fmt.Sprintf("[id=%d] %s | %s | %s\n  %s\n",
		f.ID, f.Subject, f.Category, f.CreatedAt.Format("2006-01-02"), content)
}

func formatRecallContext(facts []recallFact) string {
	if len(facts) == 0 {
		return ""
	}
	var b strings.Builder
	for i, f := range facts {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "[id=%d] %s | %s\n  %s\n", f.ID, f.Subject, f.Category, f.Content)
	}
	return b.String()
}
