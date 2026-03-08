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

// hintWriter is the minimal interface required by ExtractQueue for Stage 2.
// pgstore.SessionStore and httpclient.Client both satisfy it.
type hintWriter interface {
	StoreHint(ctx context.Context, hint memstore.ContextHint) (int64, error)
}

// ExtractQueue processes session transcripts through the FactExtractor pipeline
// after they have been saved, producing durable facts, session summaries, and
// A-MEM Zettelkasten-style links. If hintStore is non-nil, a second stage
// runs to generate context hints for the next session via the Ollama pipeline.
type ExtractQueue struct {
	extractor *memstore.FactExtractor
	store     memstore.Store
	generator memstore.Generator
	hintStore hintWriter // nil = hint generation disabled
	jobs      chan extractJob
	done      chan struct{}
	wg        sync.WaitGroup
}

// NewExtractQueue creates an ExtractQueue with a buffered job channel.
// Pass a non-nil hintStore to enable context hint generation (Stage 2).
func NewExtractQueue(store memstore.Store, embedder memstore.Embedder, generator memstore.Generator, hintStore hintWriter) *ExtractQueue {
	return &ExtractQueue{
		extractor: memstore.NewFactExtractor(store, embedder, generator),
		store:     store,
		generator: generator,
		hintStore: hintStore,
		jobs:      make(chan extractJob, 16),
		done:      make(chan struct{}),
	}
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
	query := buildSearchQuery(job.Turns)
	if query != "" {
		results, err := q.store.Search(ctx, query, memstore.SearchOpts{
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

	// Collect fact IDs for feedback tracking.
	var refIDs []string
	for _, r := range searchResults {
		refIDs = append(refIDs, strconv.FormatInt(r.Fact.ID, 10))
	}

	hint := memstore.ContextHint{
		SessionID:    job.SessionID,
		CWD:          job.CWD,
		TurnIndex:    len(job.Turns),
		HintText:     hintText,
		RefIDs:       refIDs,
		Relevance:    avgVecScore(searchResults),
		Desirability: score,
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
	return "Summarize the following conversation in 2-3 sentences. Focus on what was accomplished, " +
		"key technical decisions made, and concrete outcomes. Be specific and factual.\n\n" +
		corpus + "\n\nSummary:"
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
