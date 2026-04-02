package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/matthewjhunter/memstore"
)

// extractJob holds the data for one session to process.
type extractJob struct {
	SessionID string
	CWD       string
	Turns     []memstore.SessionTurn
}

// Hint generation constants.
const (
	hintsSearchTurns   = 3   // number of recent user turns used to build the search query
	hintsSearchMaxLen  = 200 // max bytes per user turn in the search query
	hintsSnippetTurns  = 3   // number of recent turns included in the LLM snippet
	hintsSnippetMaxLen = 500 // max bytes per turn in the LLM snippet
)

// hintRankerVersion is a monotonic version string for the hint generation pipeline.
// Increment when the Searcher, Scorer, or Synthesizer prompts change materially,
// so that historical training data can be segmented by ranker version.
const hintRankerVersion = "hint-v1"

// hintWriter is the minimal interface required by ExtractQueue for Stage 2.
// pgstore.SessionStore and httpclient.Client both satisfy it.
type hintWriter interface {
	StoreHint(ctx context.Context, hint memstore.ContextHint) (int64, error)
}

// hintRater extends hintWriter with the ability to retrieve and rate injected hints
// and facts. pgstore.SessionStore implements this. If hintStore also implements
// hintRater, ExtractQueue will auto-rate hints and facts at session end.
type hintRater interface {
	GetInjectedHints(ctx context.Context, sessionID string) ([]memstore.ContextHint, error)
	GetInjectedFactIDs(ctx context.Context, sessionID string) ([]int64, error)
	RecordFeedback(ctx context.Context, fb memstore.ContextFeedback) error
}

// backfillRater extends hintRater with query methods needed by BackfillFeedback.
// pgstore.SessionStore implements this.
type backfillRater interface {
	hintRater
	UnratedFactSessions(ctx context.Context) ([]string, error)
	GetSessionTurns(ctx context.Context, sessionID string) ([]memstore.SessionTurn, error)
}

// BackfillResult summarizes a backfill-feedback run.
type BackfillResult struct {
	Sessions int `json:"sessions"` // sessions processed
	Rated    int `json:"rated"`    // total fact ratings written
	Errors   int `json:"errors"`   // sessions that failed
}

// BackfillFeedback iterates all sessions with unrated fact injections and
// auto-rates each one. This is the batch version of autoRateFacts, used to
// bootstrap feedback scores from historical sessions.
//
// progress is called after each session with (completed, total). May be nil.
func (q *ExtractQueue) BackfillFeedback(ctx context.Context, progress func(done, total int)) (*BackfillResult, error) {
	br, ok := q.rater.(backfillRater)
	if !ok {
		return nil, fmt.Errorf("session store does not support backfill queries")
	}

	sessions, err := br.UnratedFactSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list unrated sessions: %w", err)
	}

	result := &BackfillResult{}
	for i, sessionID := range sessions {
		turns, err := br.GetSessionTurns(ctx, sessionID)
		if err != nil {
			log.Printf("backfill: session %s: get turns: %v", sessionID, err)
			result.Errors++
			if progress != nil {
				progress(i+1, len(sessions))
			}
			continue
		}

		factIDs, err := br.GetInjectedFactIDs(ctx, sessionID)
		if err != nil || len(factIDs) == 0 {
			if progress != nil {
				progress(i+1, len(sessions))
			}
			continue
		}

		snippet := buildScoreSnippet(turns)
		if snippet == "" {
			if progress != nil {
				progress(i+1, len(sessions))
			}
			continue
		}

		var facts []indexedFact
		for _, id := range factIDs {
			f, err := q.store.Get(ctx, id)
			if err != nil {
				continue
			}
			facts = append(facts, indexedFact{id: f.ID, content: f.Content})
		}
		if len(facts) == 0 {
			if progress != nil {
				progress(i+1, len(sessions))
			}
			continue
		}

		ratings, err := q.rateFacts(ctx, facts, snippet)
		if err != nil {
			log.Printf("backfill: session %s: rate facts: %v", sessionID, err)
			result.Errors++
			if progress != nil {
				progress(i+1, len(sessions))
			}
			continue
		}

		for _, r := range ratings {
			fb := memstore.ContextFeedback{
				RefID:     strconv.FormatInt(r.id, 10),
				RefType:   memstore.RefTypeFact,
				SessionID: sessionID,
				Score:     r.score,
				Reason:    r.reason,
			}
			if err := br.RecordFeedback(ctx, fb); err != nil {
				continue
			}
			result.Rated++
		}
		result.Sessions++

		if progress != nil {
			progress(i+1, len(sessions))
		}
	}
	return result, nil
}

// ExtractQueue processes session transcripts through the FactExtractor pipeline
// after they have been saved, producing durable facts, session summaries, and
// A-MEM Zettelkasten-style links. If hintStore is non-nil, a second stage
// runs to generate context hints for the next session via the Ollama pipeline.
// If hintStore also implements hintRater, a third stage auto-rates hints from
// the previous session that were injected into this one.
type ExtractQueue struct {
	extractor *memstore.FactExtractor
	store     memstore.Store
	generator memstore.Generator
	hintStore hintWriter // nil = hint generation disabled
	rater     hintRater  // nil = auto-rating disabled; set when hintStore implements hintRater
	jobs      chan extractJob
	done      chan struct{}
	wg        sync.WaitGroup
}

// NewExtractQueue creates an ExtractQueue with a buffered job channel.
// Pass a non-nil hintStore to enable context hint generation (Stage 2).
// If hintStore also implements hintRater, auto-rating of injected hints is enabled.
func NewExtractQueue(store memstore.Store, embedder memstore.Embedder, generator memstore.Generator, hintStore hintWriter) *ExtractQueue {
	q := &ExtractQueue{
		extractor: memstore.NewFactExtractor(store, embedder, generator),
		store:     store,
		generator: generator,
		hintStore: hintStore,
		jobs:      make(chan extractJob, 16),
		done:      make(chan struct{}),
	}
	if hr, ok := hintStore.(hintRater); ok {
		q.rater = hr
	}
	return q
}

// Start launches the background worker goroutine.
func (q *ExtractQueue) Start() {
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		for {
			select {
			case job := <-q.jobs:
				q.processJob(job)
			case <-q.done:
				// Drain remaining jobs before exiting.
				for {
					select {
					case job := <-q.jobs:
						q.processJob(job)
					default:
						return
					}
				}
			}
		}
	}()
}

// Stop signals the worker to finish and waits for it.
func (q *ExtractQueue) Stop() {
	close(q.done)
	q.wg.Wait()
}

// Enqueue submits a job for background processing. Non-blocking: if the buffer
// is full the job is logged and dropped rather than blocking the HTTP handler.
func (q *ExtractQueue) Enqueue(job extractJob) {
	select {
	case q.jobs <- job:
	default:
		log.Printf("extract: queue full, dropping session %s", job.SessionID)
	}
}

// processJob runs the full extraction pipeline for one session.
func (q *ExtractQueue) processJob(job extractJob) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Stage 0: auto-rate context that was injected at the start of this session.
	// Runs before extraction so failures don't block the main pipeline.
	if q.rater != nil {
		q.autoRateHints(ctx, job)
		q.autoRateFacts(ctx, job)
	}

	projectName := projectNameFromCWD(job.CWD)

	// 1. Build corpus from turns.
	corpus := buildCorpus(job.Turns)
	if corpus == "" {
		log.Printf("extract: session %s: empty corpus, skipping", job.SessionID)
		return
	}

	// 2. Metadata to attach to every extracted fact.
	metaBytes, _ := json.Marshal(map[string]string{
		"cwd":    job.CWD,
		"source": "session",
	})

	// 3. Extract facts.
	result, err := q.extractor.Extract(ctx, corpus, memstore.ExtractOpts{
		Subject: projectName,
		Hints: memstore.ExtractHints{
			Focus: []string{
				"technical decisions",
				"architecture",
				"user preferences",
				"solved problems",
				"project context",
			},
		},
		Metadata: json.RawMessage(metaBytes),
	})
	if err != nil {
		log.Printf("extract: session %s: extraction failed: %v", job.SessionID, err)
		return
	}

	// 4. Session summary.
	summaryMeta, _ := json.Marshal(map[string]string{
		"session_id": job.SessionID,
		"cwd":        job.CWD,
		"source":     "session_summary",
	})
	summary, err := q.generator.Generate(ctx, summaryPrompt(job.Turns))
	if err != nil {
		log.Printf("extract: session %s: summary generation failed: %v", job.SessionID, err)
	} else {
		summaryFact := memstore.Fact{
			Content:  summary,
			Subject:  projectName,
			Category: "project",
			Kind:     "summary",
			Metadata: json.RawMessage(summaryMeta),
		}
		if _, err := q.store.Insert(ctx, summaryFact); err != nil {
			log.Printf("extract: session %s: summary insert failed: %v", job.SessionID, err)
		}
	}

	// 5. A-MEM linking: link each new fact to related existing facts.
	linked := 0
	for _, fact := range result.Inserted {
		neighbors, err := q.store.Search(ctx, fact.Content, memstore.SearchOpts{
			Subject:    projectName,
			MaxResults: 4,
			OnlyActive: true,
		})
		if err != nil {
			continue
		}
		count := 0
		for _, r := range neighbors {
			if r.Fact.ID == fact.ID {
				continue
			}
			if r.VecScore < 0.6 {
				continue
			}
			if _, err := q.store.LinkFacts(ctx, fact.ID, r.Fact.ID, "related", true, "", nil); err != nil {
				log.Printf("extract: session %s: link %d->%d failed: %v", job.SessionID, fact.ID, r.Fact.ID, err)
				continue
			}
			count++
			if count >= 3 {
				break
			}
		}
		linked += count
	}

	errCount := len(result.Errors)
	log.Printf("extract: session %s: %d inserted, %d superseded, %d linked, %d errors",
		job.SessionID, len(result.Inserted), result.Superseded, linked, errCount)

	// Stage 2: generate context hints for the next session.
	if q.hintStore != nil {
		q.generateHints(ctx, job, projectName)
	}
}

// generateHints runs Searcher, Scorer, then Synthesizer sequentially,
// storing a ContextHint for the next session to consume.
// Operations are serialized to avoid competing for GPU memory on
// resource-constrained hardware (shared A380 across LXC containers).
// It uses its own independent 90-second timeout so Stage 1 duration doesn't
// starve Stage 2.
func (q *ExtractQueue) generateHints(_ context.Context, job extractJob, projectName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Pre-compute the snippet once; both Scorer and Synthesizer use it.
	snippet := buildScoreSnippet(job.Turns)

	// Searcher: find relevant facts given recent user messages.
	var searchResults []memstore.SearchResult
	searchQuery := buildSearchQuery(job.Turns)
	if searchQuery != "" {
		results, err := q.store.Search(ctx, searchQuery, memstore.SearchOpts{
			Subject:    projectName,
			MaxResults: 5,
			OnlyActive: true,
		})
		if err != nil {
			log.Printf("hint: session %s: searcher: %v", job.SessionID, err)
		} else {
			searchResults = results
		}
	}

	// Scorer: ask the LLM how much context injection the next session needs.
	// On error, defaults to 1 (light injection) so hints are still generated.
	score, reason, err := q.scoreDesirability(ctx, snippet)
	if err != nil {
		log.Printf("hint: session %s: scorer: %v", job.SessionID, err)
		score = 1
	}

	// Skip hint generation if nothing useful came back.
	if score < 0.5 && len(searchResults) == 0 {
		log.Printf("hint: session %s: low desirability and no relevant facts, skipping", job.SessionID)
		return
	}

	// Synthesizer: combine into a coherent context note.
	hintText, err := q.synthesizeHint(ctx, snippet, searchResults, score, reason)
	if err != nil {
		log.Printf("hint: session %s: synthesizer: %v", job.SessionID, err)
		return
	}

	// Build training data fields: all retrieved IDs, their scores, and selected IDs.
	retrievedIDs := make([]string, 0, len(searchResults))
	candidateScores := make(map[string]float64, len(searchResults))
	for _, r := range searchResults {
		idStr := strconv.FormatInt(r.Fact.ID, 10)
		retrievedIDs = append(retrievedIDs, idStr)
		candidateScores[idStr] = float64(r.VecScore)
	}
	// ref_ids (selected) == retrieved_ids here because the Synthesizer uses all
	// Searcher results. If a future Curator stage filters candidates, ref_ids would
	// be the post-filter subset and retrieved_ids would remain the full Searcher set.
	refIDs := retrievedIDs

	hint := memstore.ContextHint{
		SessionID:       job.SessionID,
		CWD:             job.CWD,
		TurnIndex:       len(job.Turns),
		HintText:        hintText,
		RefIDs:          refIDs,
		RetrievedIDs:    retrievedIDs,
		CandidateScores: candidateScores,
		SearchQuery:     searchQuery,
		RankerVersion:   hintRankerVersion,
		Relevance:       avgVecScore(searchResults),
		Desirability:    score,
	}
	if _, err := q.hintStore.StoreHint(ctx, hint); err != nil {
		log.Printf("hint: session %s: store: %v", job.SessionID, err)
		return
	}
	log.Printf("hint: session %s: stored (desirability=%.1f, refs=%d)", job.SessionID, score, len(refIDs))
}

// scoreDesirability asks the LLM to rate how much context injection the next session needs.
// Returns a score on [0, 3] and a brief reason. On LLM error, returns (1, "", err) so
// hint generation degrades gracefully to light injection rather than being skipped entirely.
func (q *ExtractQueue) scoreDesirability(ctx context.Context, snippet string) (float64, string, error) {
	prompt := `Rate the desirability of proactive context injection for the user's next coding session.

Recent conversation (last few turns):
` + snippet + `

Rate on a 0-3 scale:
0 = session complete, routine work, no follow-up expected
1 = natural continuation, some context helpful
2 = debugging in progress or problem unsolved, context clearly helpful
3 = critical error or investigation ongoing, context essential

Respond with JSON only: {"score": N, "reason": "brief explanation (max 10 words)"}`

	var raw string
	var err error
	if jg, ok := q.generator.(memstore.JSONGenerator); ok {
		raw, err = jg.GenerateJSON(ctx, prompt)
	} else {
		raw, err = q.generator.Generate(ctx, prompt)
	}
	if err != nil {
		return 1, "", err
	}

	var result struct {
		Score  float64 `json:"score"`
		Reason string  `json:"reason"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return 1, "", fmt.Errorf("parse scorer response %q: %w", raw, err)
	}
	if result.Score < 0 {
		result.Score = 0
	}
	if result.Score > 3 {
		result.Score = 3
	}
	return result.Score, result.Reason, nil
}

// synthesizeHint asks the LLM to produce a concise context note from searcher
// results and desirability score. snippet is the pre-computed turn excerpt.
func (q *ExtractQueue) synthesizeHint(ctx context.Context, snippet string, facts []memstore.SearchResult, desirability float64, reason string) (string, error) {
	factSection := "(no relevant facts retrieved)"
	if len(facts) > 0 {
		var lines []string
		for _, r := range facts {
			lines = append(lines, "- "+r.Fact.Content)
		}
		factSection = strings.Join(lines, "\n")
	}

	var urgency string
	switch {
	case desirability >= 2.5:
		urgency = "high — critical investigation in progress"
	case desirability >= 1.5:
		urgency = "medium — debugging or follow-up expected"
	default:
		urgency = "low — routine continuation"
	}
	if reason != "" {
		urgency += " (" + reason + ")"
	}

	prompt := fmt.Sprintf(`You are preparing a context note to inject at the start of the next coding session.

Recent conversation (last few turns):
%s

Relevant facts from memory:
%s

Context need: %s

Write a concise context note (2-4 sentences) that will help orient the next session.
Focus on: what was being worked on, key decisions or problems encountered, what comes next.
Be specific and actionable. No pleasantries. Plain text only.`, snippet, factSection, urgency)

	return q.generator.Generate(ctx, prompt)
}

// autoRateHints looks up hints injected at the start of this session and asks
// the LLM to rate their relevance given how the session actually unfolded.
// Ratings are written to context_feedback, providing automatic training signal
// without requiring voluntary memory_rate_context calls.
//
// Only rates hints that don't already have feedback for this session (idempotent).
// Uses a 30-second timeout independent of the main pipeline; failures are logged
// and never block extraction or hint generation.
func (q *ExtractQueue) autoRateHints(ctx context.Context, job extractJob) {
	rateCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	hints, err := q.rater.GetInjectedHints(rateCtx, job.SessionID)
	if err != nil {
		log.Printf("autoRateHints: session %s: get injected hints: %v", job.SessionID, err)
		return
	}
	if len(hints) == 0 {
		return
	}

	// Use the first few turns to evaluate hint relevance.
	snippet := buildScoreSnippet(job.Turns)
	if snippet == "" {
		return
	}

	for _, hint := range hints {
		score, reason, err := q.rateHint(rateCtx, hint.HintText, snippet)
		if err != nil {
			log.Printf("autoRateHints: session %s: hint %d: %v", job.SessionID, hint.ID, err)
			continue
		}
		fb := memstore.ContextFeedback{
			RefID:     strconv.FormatInt(hint.ID, 10),
			RefType:   memstore.RefTypeHint,
			SessionID: job.SessionID,
			Score:     score,
			Reason:    reason,
		}
		if err := q.rater.RecordFeedback(rateCtx, fb); err != nil {
			log.Printf("autoRateHints: session %s: hint %d: record feedback: %v", job.SessionID, hint.ID, err)
		}
	}
	log.Printf("autoRateHints: session %s: rated %d hint(s)", job.SessionID, len(hints))
}

// rateHint asks the LLM whether a hint was relevant given how the session unfolded.
// Returns score (+1 useful / -1 not useful) and a brief reason.
// Defaults to +1 on parse failure to avoid false negatives.
func (q *ExtractQueue) rateHint(ctx context.Context, hintText, sessionSnippet string) (int, string, error) {
	prompt := fmt.Sprintf(`A context hint was injected at the start of a coding session to help orient the work.
Rate whether the hint was relevant and useful given how the session actually unfolded.

Hint that was injected:
%s

How the session unfolded (first few turns):
%s

Rate the hint:
+1 = relevant and useful (the hint related to what was actually worked on)
-1 = not useful (the hint was off-topic, misleading, or redundant)

When in doubt, rate +1. Only rate -1 if the hint was clearly irrelevant.

Respond with JSON only: {"score": 1, "reason": "brief reason (max 10 words)"}`, hintText, sessionSnippet)

	var raw string
	var err error
	if jg, ok := q.generator.(memstore.JSONGenerator); ok {
		raw, err = jg.GenerateJSON(ctx, prompt)
	} else {
		raw, err = q.generator.Generate(ctx, prompt)
	}
	if err != nil {
		return 1, "", err
	}

	var result struct {
		Score  int    `json:"score"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return 1, "", fmt.Errorf("parse rate response %q: %w", raw, err)
	}
	if result.Score != 1 && result.Score != -1 {
		result.Score = 1 // default to useful on unexpected values
	}
	return result.Score, result.Reason, nil
}

// autoRateFacts looks up facts injected via recall at the start of this session
// and asks the LLM to rate their relevance given how the session actually unfolded.
// Ratings are written to context_feedback, closing the feedback loop for fact
// injection without requiring voluntary memory_rate_context calls.
//
// Facts are batched into a single LLM call (unlike hints which are rated one at
// a time) since individual facts are shorter than full hint texts.
func (q *ExtractQueue) autoRateFacts(ctx context.Context, job extractJob) {
	rateCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	factIDs, err := q.rater.GetInjectedFactIDs(rateCtx, job.SessionID)
	if err != nil {
		log.Printf("autoRateFacts: session %s: get injected fact IDs: %v", job.SessionID, err)
		return
	}
	if len(factIDs) == 0 {
		return
	}

	snippet := buildScoreSnippet(job.Turns)
	if snippet == "" {
		return
	}

	// Fetch fact content so the LLM can evaluate relevance.
	var facts []indexedFact
	for _, id := range factIDs {
		f, err := q.store.Get(rateCtx, id)
		if err != nil {
			log.Printf("autoRateFacts: session %s: get fact %d: %v", job.SessionID, id, err)
			continue
		}
		facts = append(facts, indexedFact{id: f.ID, content: f.Content})
	}
	if len(facts) == 0 {
		return
	}

	ratings, err := q.rateFacts(rateCtx, facts, snippet)
	if err != nil {
		log.Printf("autoRateFacts: session %s: rate facts: %v", job.SessionID, err)
		return
	}

	recorded := 0
	for _, r := range ratings {
		fb := memstore.ContextFeedback{
			RefID:     strconv.FormatInt(r.id, 10),
			RefType:   memstore.RefTypeFact,
			SessionID: job.SessionID,
			Score:     r.score,
			Reason:    r.reason,
		}
		if err := q.rater.RecordFeedback(rateCtx, fb); err != nil {
			log.Printf("autoRateFacts: session %s: fact %d: record feedback: %v", job.SessionID, r.id, err)
			continue
		}
		recorded++
	}
	log.Printf("autoRateFacts: session %s: rated %d/%d fact(s)", job.SessionID, recorded, len(facts))
}

type indexedFact struct {
	id      int64
	content string
}

type factRating struct {
	id     int64
	score  int
	reason string
}

// rateFacts asks the LLM to rate a batch of injected facts against the session
// transcript. Returns one rating per fact. Defaults to +1 on parse failure to
// avoid false negatives.
func (q *ExtractQueue) rateFacts(ctx context.Context, facts []indexedFact, snippet string) ([]factRating, error) {
	// Build the numbered fact list for the prompt.
	var factLines []string
	for i, f := range facts {
		// Truncate long facts to keep the prompt manageable.
		content := f.content
		if len(content) > 500 {
			content = content[:500] + "…"
		}
		factLines = append(factLines, fmt.Sprintf("%d. [id=%d] %s", i+1, f.id, content))
	}

	prompt := fmt.Sprintf(`These facts were injected into a coding session's context at startup:

%s

How the session actually unfolded:
%s

For each fact, rate whether it was relevant to what was actually worked on:
+1 = relevant (the fact informed or related to the session's work)
-1 = not relevant (off-topic, redundant, or never referenced)

When in doubt, rate +1. Only rate -1 if clearly irrelevant.

Respond with a JSON array only: [{"id": <fact_id>, "score": 1, "reason": "brief reason (max 10 words)"}, ...]`, strings.Join(factLines, "\n"), snippet)

	var raw string
	var err error
	if jg, ok := q.generator.(memstore.JSONGenerator); ok {
		raw, err = jg.GenerateJSON(ctx, prompt)
	} else {
		raw, err = q.generator.Generate(ctx, prompt)
	}
	if err != nil {
		return nil, err
	}

	type parsedRating struct {
		ID     int64  `json:"id"`
		Score  int    `json:"score"`
		Reason string `json:"reason"`
	}
	var parsed []parsedRating
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		// Some models return a single object instead of an array — try that.
		var single parsedRating
		if err2 := json.Unmarshal([]byte(raw), &single); err2 == nil && single.ID != 0 {
			parsed = []parsedRating{single}
		} else {
			// On parse failure, default all facts to +1.
			log.Printf("autoRateFacts: parse error %q, defaulting all to +1", raw)
			var ratings []factRating
			for _, f := range facts {
				ratings = append(ratings, factRating{id: f.id, score: 1})
			}
			return ratings, nil
		}
	}

	// Index parsed results by ID for lookup.
	parsedByID := make(map[int64]factRating, len(parsed))
	for _, p := range parsed {
		score := p.Score
		if score != 1 && score != -1 {
			score = 1
		}
		parsedByID[p.ID] = factRating{id: p.ID, score: score, reason: p.Reason}
	}

	// Build final ratings, defaulting to +1 for any fact the LLM didn't mention.
	var ratings []factRating
	for _, f := range facts {
		if r, ok := parsedByID[f.id]; ok {
			ratings = append(ratings, r)
		} else {
			ratings = append(ratings, factRating{id: f.id, score: 1})
		}
	}
	return ratings, nil
}

// buildSearchQuery builds a search query from the last hintsSearchTurns user turns.
func buildSearchQuery(turns []memstore.SessionTurn) string {
	var parts []string
	count := 0
	for i := len(turns) - 1; i >= 0 && count < hintsSearchTurns; i-- {
		if turns[i].Role == "user" {
			content := turns[i].Content
			if len(content) > hintsSearchMaxLen {
				content = content[:hintsSearchMaxLen]
			}
			parts = append(parts, content)
			count++
		}
	}
	return strings.Join(parts, " ")
}

// buildScoreSnippet returns the last hintsSnippetTurns turns as a formatted snippet for LLM prompts.
func buildScoreSnippet(turns []memstore.SessionTurn) string {
	start := len(turns) - hintsSnippetTurns
	if start < 0 {
		start = 0
	}
	var lines []string
	for _, t := range turns[start:] {
		content := t.Content
		if len(content) > hintsSnippetMaxLen {
			content = content[:hintsSnippetMaxLen] + "…"
		}
		lines = append(lines, "["+t.Role+"]: "+content)
	}
	return strings.Join(lines, "\n")
}

// avgVecScore returns the mean vector similarity score across search results.
func avgVecScore(results []memstore.SearchResult) float64 {
	if len(results) == 0 {
		return 0
	}
	var sum float64
	for _, r := range results {
		sum += r.VecScore
	}
	return sum / float64(len(results))
}

// buildCorpus joins session turns into a single text corpus truncated to ~32 KB.
// Turns are consumed from the tail (most recent first) so that early large turns
// — file reads, pastes — don't crowd out the decisions and outcomes that follow.
// Each individual turn is also capped at maxTurnBytes to prevent a single massive
// response from consuming the entire budget.
func buildCorpus(turns []memstore.SessionTurn) string {
	const (
		maxBytes     = 32 * 1024
		maxTurnBytes = 4 * 1024
	)

	// Collect chunks from newest to oldest, then reverse for chronological output.
	var chunks []string
	remaining := maxBytes
	for i := len(turns) - 1; i >= 0; i-- {
		content := turns[i].Content
		if len(content) > maxTurnBytes {
			content = content[:maxTurnBytes] + "…"
		}
		chunk := "[" + turns[i].Role + "]: " + content
		if len(chunk)+5 > remaining { // 5 for "\n---\n"
			break
		}
		chunks = append(chunks, chunk)
		remaining -= len(chunk) + 5
	}

	// Reverse to restore chronological order.
	for l, r := 0, len(chunks)-1; l < r; l, r = l+1, r-1 {
		chunks[l], chunks[r] = chunks[r], chunks[l]
	}
	return strings.Join(chunks, "\n---\n")
}

// summaryPrompt builds a prompt asking the LLM to summarize the session.
// Reuses buildCorpus so the same size guards apply — no separate turn count
// or byte limit needed here.
func summaryPrompt(turns []memstore.SessionTurn) string {
	corpus := buildCorpus(turns)
	return `Summarize this conversation in ≤150 words. Structure as a short list:
- Lead with one sentence stating what the session was about.
- Bullet each key decision or concrete outcome (what was decided, not the process).
- Omit process narration ("the assistant then…", "the conversation focused on…").
- Use concrete names (functions, files, flags) instead of generic descriptions.
- If nothing was decided or accomplished, the topic sentence alone is sufficient.

` + corpus + "\n\nSummary:"
}

// projectNameFromCWD walks up from cwd looking for a .git directory and returns
// the base name of that directory. Falls back to filepath.Base(cwd).
func projectNameFromCWD(cwd string) string {
	if cwd == "" {
		return "unknown"
	}
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return filepath.Base(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Base(cwd)
}
