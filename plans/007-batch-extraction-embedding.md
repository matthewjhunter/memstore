# Plan 007: Batch the embedding calls in the extraction pipeline

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md` -- unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat b6a3d4f..HEAD -- extract.go extract_test.go httpapi/extractqueue.go httpapi/extractqueue_test.go`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P2
- **Effort**: M
- **Risk**: MED
- **Depends on**: none (interacts with nothing in plans 001-006)
- **Category**: perf
- **Planned at**: commit `b6a3d4f`, 2026-06-12

## Why this matters

Extracting N facts from a session currently makes ~3N serial embedding HTTP
calls, and every one of them embeds a string the pipeline already embedded:

1. `extract.go:178` -- `embedding.Single(ctx, e.embedder, ef.Content)` once
   per fact.
2. `extract.go:221` -- `trySupersedeExisting` calls `store.Search` with the
   same content; `Search` embeds its query internally (`search.go:30`).
3. `httpapi/extractqueue.go:271-275` -- the A-MEM linking stage calls
   `store.Search(ctx, fact.Content, ...)` per inserted fact: a third
   embedding of the same string.

At ~100-500ms per round-trip against a local Ollama, a 40-fact session pays
roughly 120 serial calls -- minutes of wall-clock in the extract queue,
delaying hint generation and next-session context. The store interface
already has the right tool: `SearchBatch` "shares a single batched embedding
call across queries" (`store.go`, implemented at `search.go:291` and
`pgstore/search.go:72`). Restructuring the loop into phases cuts ~3N calls
to roughly 3 batched calls total.

## Current state

- `extract.go:119-205` -- `FactExtractor.Extract`. One loop per extracted
  fact: resolve subject/category, `Exists` dedup check, `embedding.Single`,
  `Insert`, then `trySupersedeExisting`. Per-fact errors are appended to
  `result.Errors` and the loop continues; a fact whose embedding fails is
  NOT inserted.
- `extract.go:215-257` -- `trySupersedeExisting(ctx, newFact)` (unexported
  method, free to reshape). It searches same-subject active facts
  (`MaxResults: 10, Subject: newFact.Subject, OnlyActive: true`), skips
  self, skips empty candidate embeddings, skips `MetadataConflicts`, picks
  the best cosine similarity, and supersedes when `>= similarityThreshold`
  (0.85, `extract.go:38`). Sequencing today: fact i is searched after facts
  0..i are inserted and after earlier supersessions have landed -- so
  candidates never include facts i+1..N, and never include facts already
  superseded earlier in the same run.
- `extract.go:77-116` -- `ExtractFacts` (exported, non-persisting): no
  embedding involvement. Do not touch.
- `httpapi/extractqueue.go:269-298` -- the A-MEM linking stage:

  ```go
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
          if r.Fact.ID == fact.ID { continue }
          if r.VecScore < 0.6 { continue }
          if _, err := q.store.LinkFacts(fact.ID, r.Fact.ID, "related", true, "", nil); ... }
          count++
          if count >= 3 { break }
      }
      linked += count
  }
  ```

  Note the opts are IDENTICAL for every fact here (single subject:
  `projectName`) -- a perfect `SearchBatch` fit.
- `search.go:291-340` -- SQLite `SearchBatch`: embeds all queries via one
  `embedding.EmbedWithRetry`, then runs FTS+vector per query under one
  RLock. Deliberately never reranks ("the bulk/backfill path").
  `pgstore/search.go:72+` is equivalent (uses the query-embedding cache).
  Neither `trySupersedeExisting` nor the A-MEM stage uses reranking today
  (default `SearchOpts`), so switching to `SearchBatch` does not change
  scoring behavior.
- `embedding.EmbedWithRetry(ctx, embedder, texts)` is already used at
  `sqlite.go:950`; go-embedding handles retry and batch-shrink internally.
- Tests: `extract_test.go` has a `mockGenerator` (line 14) and the
  behavior suite to preserve -- especially `TestExtractDedup`,
  `TestExtract_AutoSupersede_AboveThreshold` / `_BelowThreshold` /
  `_ConflictingMetadata` / `_DifferentSubjects` / `_NilEmbedder`.
  `httpapi/extractqueue_test.go` covers the queue.

## Commands you will need

| Purpose | Command                                                        | Expected on success |
|---------|----------------------------------------------------------------|---------------------|
| Build   | `GOWORK=off go build ./...`                                    | exit 0              |
| Focused | `GOWORK=off go test -race -count=1 -run Extract ./... `        | ok                  |
| Tests   | `GOWORK=off go test -race -count=1 ./...`                      | all packages ok     |
| Vet     | `GOWORK=off go vet ./...`                                      | exit 0              |
| Format  | `test -z "$(gofmt -l .)"`                                      | exit 0              |

## Scope

**In scope** (the only files you should modify):
- `extract.go`
- `extract_test.go`
- `httpapi/extractqueue.go` (the A-MEM linking stage only)
- `httpapi/extractqueue_test.go`

**Out of scope** (do NOT touch, even though they look related):
- `ExtractFacts` (the exported non-persisting variant) -- unchanged.
- `search.go` / `pgstore/search.go` -- `SearchBatch` is consumed as-is.
- A search-by-vector Store capability (which would eliminate re-embedding
  entirely by reusing the phase-B vectors) -- deliberately deferred; it is
  interface surgery across both backends, httpclient, and httpapi, and the
  batching here already removes ~99% of the round-trips. See maintenance
  notes.
- `cmd/memstore/learn_cmd.go`, `mcpserver/` -- they call the library and
  benefit automatically.
- Stages 2/3 of the extract queue (hints, rating) and `summarizeAndPersist`.

## Git workflow

- Branch off `main`: `advisor/007-batch-extraction-embedding`
- Suggested commits: `extract: batch embedding and supersession search`,
  then `extractqueue: batch the A-MEM linking searches`
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Restructure Extract into phases

Rewrite the loop in `Extract` (`extract.go:143-202`) as four phases.
Preserve every per-fact error message format exactly (tests may match on
them, and operators grep logs).

**Phase A -- resolve and dedup.** Build a `candidates` slice: skip empty
content; resolve subject/category defaults; run the `Exists` check (same
error handling: append error + skip fact). CRITICAL: also dedup within the
batch -- today the second copy of a duplicated content is caught by
`Exists` because the first was already inserted; with inserts deferred,
that no longer happens. Keep a `seen map[string]bool` keyed on
content+"\x00"+subject; in-batch repeats increment `result.Duplicates`
exactly as DB-level duplicates do.

**Phase B -- batch embed.** If `e.embedder != nil` and candidates remain:

```go
contents := ... // one per candidate, in order
embs, err := embedding.EmbedWithRetry(ctx, e.embedder, contents)
```

On error: append one error (`fmt.Errorf("memstore: batch embedding %d facts: %w", len(contents), err)`)
and return the result with no inserts -- this matches the existing rule
that a fact without its embedding is not inserted, applied batch-wide.
Verify `len(embs) == len(contents)` (mismatch = same treatment as error;
model the check on `sqlite.go:955-957`). Assign `candidates[i].Embedding =
embs[i]`.

**Phase C -- insert.** Per candidate: `Insert` with the same per-fact
error-and-continue handling as today; record the ID; append to
`result.Inserted`.

**Phase D -- supersession.** See step 2.

**Verify**: `GOWORK=off go build ./...` -> exit 0 (step 2 may be required
first if trySupersedeExisting's signature changed -- do steps 1+2 as one
edit if simpler, then verify once)

### Step 2: Batch the supersession searches

Replace the per-fact `store.Search` inside `trySupersedeExisting` with
per-subject `SearchBatch` calls issued once, after phase C:

1. Group inserted facts by subject (preserve each fact's original batch
   index). `SearchOpts.Subject` is single-valued, so one `SearchBatch` per
   distinct subject: `SearchBatch(ctx, contentsOfThatSubject,
   SearchOpts{MaxResults: 10, Subject: subj, OnlyActive: true})`.
   A `SearchBatch` error fails supersession for that subject group only:
   append one error, continue with other groups (mirrors today's per-fact
   error tolerance).
2. Process facts in original insertion order. Refactor
   `trySupersedeExisting` to take pre-fetched candidates:
   `trySupersedeFrom(newFact Fact, candidates []SearchResult, batchOrder map[int64]int, supersededInRun map[int64]bool) (*int64, error)`
   (exact signature is the executor's call; it is unexported). Inside, keep
   the existing filters verbatim -- skip self, skip empty embedding, skip
   `MetadataConflicts` -- and add two new ones that preserve today's
   sequential semantics:
   - skip candidates that are batch-mates with a LATER batch index than the
     current fact (under the old code they did not exist yet when this fact
     was searched); without this, two near-duplicate facts in one batch can
     supersede each other and create the A->B->A cycle plan 005 guards
     reads against -- this plan must not create them;
   - skip candidates in `supersededInRun` (under the old code,
     `OnlyActive` had already filtered them).
3. On a supersession, add the superseded ID to `supersededInRun` and
   increment `result.Superseded` -- same as today.
4. Keep the `e.embedder == nil || len(newFact.Embedding) == 0` early-out:
   with a nil embedder, phase D is skipped entirely
   (`TestExtract_AutoSupersede_NilEmbedder` pins this).

Accepted behavioral delta (do not "fix" it): candidate lists are now
computed after ALL batch inserts, so a fact's 10-result pool can be crowded
by later batch-mates that the two filters then discard. With MaxResults 10
and conservative 0.85 threshold this is negligible; it is noted here so a
reviewer knows it was considered.

**Verify**: `GOWORK=off go test -race -count=1 -run Extract ./` -> ok --
ALL existing AutoSupersede/Dedup tests pass unchanged

### Step 3: Batch the A-MEM linking stage

In `httpapi/extractqueue.go:269-298`: one call replaces the per-fact loop's
searches --

```go
contents := make([]string, len(result.Inserted))
for i, f := range result.Inserted { contents[i] = f.Content }
neighborSets, err := q.store.SearchBatch(ctx, contents, memstore.SearchOpts{
    Subject:    projectName,
    MaxResults: 4,
    OnlyActive: true,
})
```

On error, log (`log.Printf("extract: session %s: link search failed: %v", ...)`,
matching the file's log style) and skip the linking stage. Then keep the
inner per-fact filtering loop exactly as it is today (skip self, skip
`VecScore < 0.6`, cap 3 links per fact, count `linked`), iterating
`neighborSets[i]` for `result.Inserted[i]`.

**Verify**: `GOWORK=off go test -race -count=1 ./httpapi/` -> ok

### Step 4: Full suite + count check

**Verify**: `GOWORK=off go test -race -count=1 ./...` -> all ok;
`test -z "$(gofmt -l .)"` -> exit 0;
`grep -n "embedding.Single" extract.go` -> no matches.

## Test plan

New tests in `extract_test.go` (model setup on the existing AutoSupersede
tests, which build a store + `mockGenerator`):

- `TestExtract_EmbedderCallCount`: wrap the test embedder in a counting
  embedder (increment a counter in `Embed`). Extract 3 facts sharing one
  subject. Expect exactly 2 `Embed` calls: one phase-B batch + one
  `SearchBatch` (the old code made 6+). Assert the count, not timing.
- `TestExtract_InBatchDuplicates`: generator returns the same content+
  subject twice; expect 1 insert and `Duplicates == 1`.
- `TestExtract_InBatchSupersessionNoCycle`: generator returns two
  near-identical contents (craft the mock embedder so their vectors have
  cosine >= 0.85, same subject, no metadata conflict). Expect: the later
  fact supersedes the earlier one (Superseded == 1), the earlier fact is
  inactive, the later fact is active, and `History` on either returns a
  finite chain with no fact superseded-by itself.
- `TestExtract_BatchEmbedFailure`: embedder returns an error; expect zero
  inserts, `result.Errors` non-empty, and no panic.

In `httpapi/extractqueue_test.go`:

- Check whether a linking-stage test exists (`grep -n "LinkFacts\|linked"
  httpapi/extractqueue_test.go`); extend or add one asserting links are
  still created for inserted facts after the batch change, and that a
  failing search skips linking without failing the job.

Existing tests: the entire `-run Extract` suite and
`httpapi/extractqueue_test.go` must pass WITHOUT modification. If one needs
its assertions changed, that is a STOP condition, with one exception: a
test that asserts the exact number of embedder calls under the old serial
scheme may be updated, with a comment, since call count is the thing this
plan changes on purpose.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `GOWORK=off go test -race -count=1 ./...` exits 0, including the 4+ new tests
- [ ] `grep -n "embedding.Single" extract.go` returns no matches
- [ ] `grep -c "store.Search(" httpapi/extractqueue.go` shows the per-fact Search in the linking stage is gone (SearchBatch present instead)
- [ ] Existing AutoSupersede/Dedup/extractqueue tests pass unmodified (except a call-count assertion, if any, per the test plan)
- [ ] `test -z "$(gofmt -l .)"` exits 0
- [ ] No files outside the in-scope list are modified (`git status`)
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- `Extract` or the A-MEM stage no longer match the "Current state" excerpts.
- An existing test fails in a way that requires changing its assertions
  (other than the embedder-call-count exception above) -- the phases are
  meant to be behavior-preserving; a failing assertion means a semantic was
  missed. Report which one.
- Preserving the sequential supersession semantics (step 2's two filters)
  turns out to require more state than described -- e.g. you find a path
  where candidate ordering still permits a cycle. Report the scenario; do
  not invent additional filters.
- `SearchBatch` on either backend behaves differently than documented here
  (e.g. reranks, or returns results misaligned with query order).

## Maintenance notes

- The remaining re-embedding (SearchBatch embeds contents that phase B
  already embedded) is the cost of avoiding a Store interface change. The
  clean end-state is a search-by-vector capability interface (the
  tier1-graph-basics.md doc already proposes capability interfaces as the
  pattern); when that lands, phase D and the A-MEM stage should pass the
  phase-B vectors instead of re-embedding. File it as a follow-up, do not
  do it here.
- Reviewers should scrutinize step 2's two ordering filters -- they are
  the only subtle logic in this plan, and the no-cycle test is the guard.
- If extraction ever runs concurrently for the same namespace, the
  `supersededInRun` set stops being sufficient (another run can supersede
  candidates mid-flight); the extract queue is single-worker today, which
  is what makes this safe.
