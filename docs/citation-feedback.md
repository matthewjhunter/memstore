# Citation feedback -- scope

Status: **proposed**, 2026-09-10. No code yet. Follows #162 (the citation convention), #161 (recall feedback on frozen ratings), and #217 (citing a memory that turned out wrong). Decisions are marked D1-D4 below.

## Why citations

Recall needs to know which facts get used. Three producers exist. Two have never been called, and the third is an LLM's judgement of what hook recall injected:

- `memory_rate_context` is model-initiated and has never been called.
- `memory_confirm` is model-initiated and has never been called (#160).
- The extract-queue auto-rater has an LLM judge each fact that hook recall injected. It writes again since transcript upload was revived in August, and it is the only live producer.

#162 added a fourth that costs the model no separate action: when a memory shapes an answer, cite it inline as `[fact N]`. The rationale in `mcpserver/instructions.go` makes it positive-only on purpose. A convention like "prefer small commits" shapes a whole response without being quotable, so a missing citation carries no meaning, and treating it as evidence against a fact would penalise the preference layer memstore exists for.

## What exists

Measured on production on 2026-09-10.

- The instruction shipped on 2026-08-23. Since then, 44 `[fact N]` strings appear in recorded assistant turns, across 18 sessions.
- 5 of the 44 are the instruction's own example ids (`[fact 1234]`, and `[fact 9999]` from a code comment), quoted in conversations about the convention itself.
- 39 resolve to real facts. 10 of those had been injected by hook recall in the same session. The other 29 have no recorded exposure; they are mostly decision, pattern, invariant and convention facts, the kinds file-trigger context surfaces, plus one startup task.
- Nothing reads citations back. `CitationPattern` is used only in tests.
- Recall's feedback multiplier reads `context_feedback`. In September the auto-rater wrote 212 fact ratings, 20% of them positive.

## Problems

**1. The instruction names the wrong source for ids.** It says to cite "an id listed in that result's `framing` field", which describes an MCP envelope. Hook recall and file-trigger context deliver facts with memstore's own `[id=N]` label, outside the fence. Read literally, those cannot be cited, and they are where most facts arrive.

**2. The example parses as a citation.** `[fact 1234]` in the instruction matches `CitationPattern`, so any conversation about the convention produces citations of an id that was never shown.

**3. A memory that was wrong is not clearly covered (#217).** A stale fact recognised and corrected shaped the answer as much as a correct one, and its id is what a reader needs to fix the record.

**4. There is no reader.** The model writes the signal into the transcript, the transcript is uploaded, and the signal is dropped.

**5. Exposure is logged on one channel.** Hook recall records `context_injections`. File-trigger context (`memstore-read.mjs`, `memstore-edit.mjs`) and the startup task list record nothing, although both hooks have the session id in hand. MCP tool results cannot be tied to a Claude Code session at all: the handlers never see its `session_id`.

**6. The auto-rater's rubric contradicts the convention.** `rateFact` scores -1 for a fact that was "off-topic, redundant, or never referenced". "Never referenced" is the absence the convention says carries no meaning, so the rater demotes conventions and preferences in particular. Age decay (#161) limits how long a rating holds; it does not change what the rater writes.

## Proposal

### 1. Instruction wording

- Ids may come from an envelope's `framing` field or from the `[id=N]` label memstore puts outside the fence in hook-injected context. Never from inside a fence.
- Show the form as `[fact N]`, which `CitationPattern` cannot match. `TestCitationPatternMatchesTheDocumentedForm` changes to pin that a numeric instance matches and the placeholder does not.
- Add #217's case: a memory recognised as stale, contradicted or corrected shaped the answer and is cited on the same terms.

Changing the wording now disturbs only three weeks of data.

### 2. A reader in the daemon

On `POST /v1/sessions/transcript`, after `SaveTurns`, scan the assistant turns with `CitationPattern` and keep ids that resolve to an active fact of the calling user. Record each as a row in a new `fact_citations` table (session id, fact id, turn uuid, cited at, user id), unique on session and fact, and bump new `cite_count` and `last_cited_at` columns on `memstore_facts`. The unique key makes a re-uploaded or resumed session safe to scan again.

This follows #158: the daemon already holds the transcript, so no client has to remember to report anything.

The new columns fall under the invariants in `CLAUDE.md`: `factColumns` and `scanFact`, `searchFTS`'s column list, and the transfer scan all change together.

A one-time admin pass reads the existing session turns, with a dry-run count first.

### 3. Exposure logging where a session id exists

- File-trigger context: `eval-triggers` takes the session id and records the facts it returns as injections.
- The startup task list records the task ids it shows.
- MCP results stay unverifiable, and citations of facts surfaced there are accepted (D2).

With exposure recorded, two measurements become possible: cite rate per channel (citations over exposures), and citations with no recorded exposure, as a check on invented ids.

### 4. What the signal is for, in order

First, measurement: is recall used at all, on which channels, and for which kinds of fact. That is the question the convention was built to answer, and it needs weeks of data.

Ranking comes after, and only on that data. Candidates are a positive-only boost from citations relative to exposures, or replacing the auto-rater's fact ratings with citations outright. Neither should be picked before there are a few hundred citations to look at.

## Decisions

**D1. Where citations are stored.** A separate `fact_citations` table plus counters, or rows in `context_feedback`. Recommend the separate table. `context_feedback` is unique on reference, type and session, so a citation would collide with the auto-rater's rating of the same fact in the same session, and mixing an observed citation with a model's judgement loses which is which.

**D2. Citations with no recorded exposure.** Accept them, or require exposure. Recommend accepting any id that resolves to an active fact of the user, and reporting unexposed citations separately. MCP makes exposure unverifiable for a whole channel, and the 29 unexposed citations measured so far are real facts, not inventions.

**D3. The auto-rater.** (a) Change its rubric so that "never referenced" is neutral and -1 is reserved for a fact that was wrong or misleading in context; (b) stop it rating facts once citations are being read; (c) leave it. Recommend (a) now. It is one prompt change, it stops the live demotion of conventions, and it keeps the only working producer running while citations accumulate.

**D4. The example form.** `[fact N]`, or keep a numeric example and ignore that id when reading. Recommend `[fact N]`. An ignore list is one more thing to keep in step, and any number chosen will eventually be a real fact.

## Out of scope

- `memory_confirm`, which has its own decision in #160.
- Hints: they carry hint ids, not fact ids, and are not citable under the convention.
- Document chunks: they have their own id space, and `SealKind` already keeps chunk ids from being offered as fact ids.

## Measurement plan

Weekly, from `fact_citations` and `context_injections`:

- citations, and sessions citing
- cite rate: sessions citing over sessions with recorded exposure, per channel
- citations by fact kind
- citations of ids with no recorded exposure, and ids that do not resolve

Revisit the ranking question once there are a few hundred citations.
