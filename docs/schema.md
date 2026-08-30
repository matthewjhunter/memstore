# The schema: what kinds of thing memstore holds

Status: draft. Taxonomy half only -- conventions and workflows are a second
pass. Revised 2026-08-30 to add the domain and sensitivity axes.
Author: Matthew + Claude
Date: 2026-08-30

The third layer of the pattern in `docs/wiki-organizer.md`. Karpathy describes
it as a document that "tells the LLM how the wiki is structured, what the
conventions are, and what workflows to follow", co-evolved with the model rather
than authored once.

This is the first half: what kinds of thing the store holds, and what each kind
implies. The conventions (what a subject is, when a group of facts earns a
synthesis page, whether reassignment supersedes or updates) and the workflows
are deliberately not here; they are orthogonal and mixing them in obscures both.

Nothing in this document is enforced by code yet. It describes the intended
shape so the organizer's prompts have something consistent to read, and so the
gap between intent and the live corpus is visible. The migration is noted at the
end.

## The organizing principle is not topic

Categories here are not subject areas. They are answers to a single question:
**what makes an entry wrong, and who is therefore allowed to fix it.**

| category | wrong means | corrected by | may a model supersede it |
|---|---|---|---|
| research | it misrepresents the source | re-reading the source | yes, mechanically |
| project | the world changed | re-observing the world | yes, on new observation |
| general | the user says so | the user | no |
| transcript | *not a category* -- it happened | nothing; immutable | no |
| fiction | inconsistent with the work | the user, or the work | only within its world |

Topic and area of life are real and useful ways to slice the corpus, and they
get their own axes below -- domain and sensitivity. They are not this axis.
Topic taxonomies say nothing about any of that. This one determines directly
what contradiction lint means for an entry, what reconciliation is permitted,
and whether the organizer may touch it at all -- which is every question the
operations in `wiki-organizer.md` need answered.

## Two layers, before the categories

Two of the five are not claims at all.

A research paper is not an assertion; it is a document that contains
assertions. A session transcript is not an assertion either; it is a record of
what was said. Both are verbatim source material and belong in
`memstore_documents` under the invariant in `docs/document-corpus.md`: a chunk's
bytes are identical to a span of the recorded file at the recorded hash.

The other three are claims and belong in `memstore_facts`.

| category | layer |
|---|---|
| research | documents, plus derived facts |
| project | facts |
| general | facts |
| transcript | documents, plus derived facts |
| fiction | facts, in their own namespace |

Research and transcripts appear in both layers because stage two reads their
chunks and writes derived facts citing them. The derived fact is what recall
returns; the document is what it stays checkable against. Storing the paper
alone would leave us doing retrieval at query time, which the pattern argues
against; storing only the summary would leave nothing to check it against.

## The categories

### research

External material, found and handed over deliberately, that represents an idea
worth keeping: papers, articles, specifications, gists. Predominantly AI,
computer security, programming, hardware, and computer use generally.

The purpose is retention rather than reference. Once ingested, the original
should not need relocating: the ideas in it are searchable, linked to related
concepts, and applicable later without the source in hand.

- **Layer:** verbatim document, plus derived facts citing its chunks.
- **Wrongness:** a derived fact is wrong when it misrepresents the source. The
  source itself cannot be wrong in the store's sense; it is what it is, and may
  simply be mistaken about the world, which is a different thing.
- **Correction:** re-read the chunks. This is the one category where the model
  can fully adjudicate its own errors, because the evidence is present.
- **Organizer:** free rein. Summarize, revise, link, supersede its own output.

Worked examples: Karpathy's LLM Wiki gist, which is the source of this whole
design and which we have referred to repeatedly from memory rather than from a
stored copy; and the paper on embedding chunk sizes, whose finding was that
small consistent chunks outperform large ones. Both are exactly the shape this
category exists for -- broad topical knowledge, applied long after reading, from
a source nobody wants to go find again.

### project

The working context of a specific project: where things are, how they are
reached, what is true about their construction, and what constraints they
operate under.

- **Layer:** facts.
- **Wrongness:** the world changed. A project fact is not refuted, it goes
  stale. An IP that moved was never a lie.
- **Correction:** re-observation. New evidence supersedes old, and the model may
  perform the supersession when it has actually observed the change.
- **Organizer:** may cluster, name, link, and write synthesis pages. Should not
  invent project facts from inference; a project claim should trace to an
  observation.
- **Boundary:** what a repository already records about itself -- architecture,
  invariants, conventions -- is authoritative in that repository's own
  `CLAUDE.md` and code, not here. This category holds what travels across repos
  or is not written down in one.

Worked examples: the repository layout convention (a top-level git directory,
then the GitHub account or organization, then the repo name, so any repo is
locatable from its owner and name); the homelab inventory, its hardware and
network layout and how each system is reached; the maildancer access
invariants, which restrict which components may reach which others so that the
IMAP daemon cannot read mail stores off disk instead of going through the mail
session.

That last example is worth noting as the shape to aim for: it is a constraint
with a reason attached, and the reason is what makes it survivable when someone
later wonders why the indirection exists.

### general

The closest analogue to human memory, and the reason memstore exists. Who the
user is, what has been discussed, opinions held, people, history, chronology.
The slice of context that should follow the user across every session and every
working directory.

- **Layer:** facts.
- **Wrongness:** the user says so. Nothing else settles it.
- **Correction:** the user, only. **A model may never supersede a fact in this
  category.** It may flag a contradiction and show it; it may not resolve one.
- **Organizer:** may cluster, name, and link. May write synthesis pages *over*
  general facts, since a page is a derived fact and does not overwrite its
  sources. May not rewrite or supersede the facts themselves.

This is the category the reconciliation rule in `docs/document-synthesis.md`
was written to protect, and the restriction is not a nicety. A model that
quietly corrects a person's memory of their own life produces a store the person
cannot trust, and an untrusted store is worse than no store.

### transcript

Session transcripts, ingested so that past work is verifiable rather than
recalled. The goal is that a session can be located by search or by listing,
carries a topical description of what happened in it, and can be read directly
when something needs checking against what was actually said.

- **Layer:** verbatim document, plus derived facts (a per-session summary,
  chronologically placed).
- **Wrongness:** not applicable. A transcript records what happened. It can be
  incomplete or badly summarized, but it cannot be false.
- **Correction:** none. Immutable, like any document.
- **Organizer:** may summarize and link. May never edit, and may never supersede
  a transcript-derived fact with an inference.

**Transcripts are the highest-risk material in the store and get a stricter
default than any other category.** Every credential, token, connection string
and secret that has ever appeared in a session appears in its transcript. The
screening machinery already exists -- mechanical detection through
`airlock/detect`, a model pass behind it, screen states on facts, separate
write-time and read-time detect modes, a background worker, and the `memstore
scan` command -- and has never been run against transcript material at ingest
scale.

Three rules follow, and they are the reason this category is called out
separately rather than folded into research:

1. **Quarantine before landing, not screening after.** Other material is
   screened as it settles. Transcripts are screened before they are readable at
   all, because the cost of a miss is a live credential in a searchable corpus.
2. **Mechanical first, model second, and never model only.** A detector that
   misses is the expected case, so the model pass is a second net rather than
   the primary one. Neither is treated as sufficient alone.
3. **Not in default recall.** Transcripts are reachable by deliberate search,
   not injected by ambient retrieval. This bounds the blast radius of a miss to
   a query someone chose to run.

Volume is the open question rather than a settled rule: six months of sessions
is a large corpus, and whether everything is ingested or only sessions that
produced something should be measured before it is decided.

### fiction

Claims that are true only inside an invented world. The purpose is writing:
a novel can be ingested chapter by chapter and then searched for a character's
physical description, their history, the events on their timeline, or what was
established about a place.

- **Layer:** facts, **in their own namespace.**
- **Wrongness:** inconsistent with the work. Not false -- inconsistent.
- **Correction:** the user, or a later passage of the work itself.
- **Organizer:** may cluster, name, link and summarize within a world. Must not
  run contradiction lint the way it runs elsewhere; see below.

**Why a namespace and not a category.** Fiction is the only material whose truth
is scoped, and memstore is namespace-scoped at store construction, so a
namespace makes leakage structurally impossible rather than a matter of
filtering carefully. The failure mode this prevents is not hypothetical in
either direction: a character's invented biography surfacing in a recall about
real people is the obvious one, and the subtler one is contradiction lint firing
on every character who is described differently in chapter 2 and chapter 40 --
which is either an error or characterisation, and no model can tell which.

Within the namespace, subjects are hierarchical, coarse to fine: world, then
series, then book, then chapter. Timeline position is in-world chronology, not
insert time, which is what the `occurred_at` column below exists for.

**What is not fiction.** Material *about* fiction is not fiction. The test is
whether the claim is true in our world:

- a book on prose style, plot construction, or character work is **research** --
  it is true here, and putting it in the fiction namespace would make it
  invisible exactly when it is wanted;
- discussion of a real author's real books, and opinions about them, is
  **general** -- the works of Daniel Keys Moran, what is good in them and why,
  are real claims about real books;
- only a claim that holds solely within an invented world is **fiction**.

## The second axis: domain

Business and personal are real and needed, and they are not categories. The
wrongness test says why: what makes a claim about the house search wrong, or
about the osg site, or about a bank account? The world changed. That is the
project answer, and it is the same answer for all of them. They do not divide
the corpus by who may correct what, which is what the category axis is for.

What they divide it by is **domain** -- the area of life or work a fact belongs
to. That is a second axis, and it already has a home: the subject hierarchy.

    business/osg              business/sf            business/maildancer
    personal/house            personal/job           personal/finance

This is strictly better than a flat category slot would be, because domains have
depth and categories cannot. `personal/job/applications` and
`personal/job/interviews` are natural; a `personal` category with 400 facts in
it is another dumping ground of exactly the kind `wiki-organizer.md` exists to
break up.

The two axes are independent and both apply to every fact. A fact can be
category `project`, domain `business/maildancer`. Another can be category
`general`, domain `personal/house` -- an opinion about a neighbourhood is a
general fact that only the user may correct, and it is also part of the house
search.

Domain is an open vocabulary. Category is closed. That asymmetry is deliberate:
new areas of life appear constantly and cost nothing, while a new answer to
"who may correct this" is a change to how the organizer is allowed to behave.

## The third axis: sensitivity

Naming domain exposes a third axis that the transcript rules above were already
using without naming it. "Not in default recall" is not a statement about how
transcripts can be wrong. It is a statement about what it costs to surface them
in the wrong place.

Sensitivity governs retrieval and visibility, not correctness:

- **ambient** -- eligible for recall injection at session start and on any
  query. The default, and where most project and general material sits.
- **on request** -- reachable by deliberate search, never injected. Transcripts,
  for the credential-exposure reason given above.
- **restricted** -- reachable, but excluded from anything that leaves the
  machine. `personal/finance` and medical material belong here, and the
  existing rule that personal and medical details never appear in published
  writing is exactly this predicate applied by hand today.

Business is worth a decision rather than a default. Today the team is two, so
nothing turns on it; the reason to mark it now is that a visibility predicate
retrofitted onto an established corpus is a migration, while one carried from
the start is a column.

This axis is the same predicate the tier 3 multiuser work needs, approached from
the single-user side. It should not be built twice.

## Not in the wiki: tasks

Tasks are the largest single subject in the live corpus (350 facts on `todo`)
and they are none of the five. They are operational state with a lifecycle, and
a wiki page has no status transitions.

They are named here so that the organizer excludes them explicitly rather than
attempting to cluster and synthesize `todo` into a page. Task metadata,
selection and surfacing are their own mechanism and are unaffected by anything
in this document.

## Chronology: when it happened, not when we heard it

Facts carry `created_at`, which is when the store learned something. Nothing
records when the thing itself occurred.

Two categories need the distinction. A general fact about an event in 2019 is
not a fact from 2019. A fiction fact about chapter 12 sits at a point in an
in-world timeline that has no relationship to insert order at all, since books
are not written in chronological order.

One nullable `occurred_at` column serves both, and without it "organized by
chronology" is not expressible for either. Subject to the four-place update rule
(`factColumns`, `scanFact`, `searchFTS`, `ExportedFact`).

Its meaning differs by namespace -- real time in the default namespace, in-world
time in a fiction namespace -- and that is acceptable because the namespace is
what scopes truth in the first place.

## Open: instructions are not claims

Tone rules, punctuation conventions, publishing etiquette, and working
preferences are directives rather than assertions about the world. They cannot
be contradicted, only superseded, so the wrongness test that organizes this
document does not apply to them.

Some of them already live in the user's global `CLAUDE.md`, which raises a
question this draft does not settle: which surface owns them. Duplicating them
into memstore gives two sources that can disagree; leaving them only in
`CLAUDE.md` means they do not travel and cannot be searched.

Deferred to the conventions pass.

## What this replaces

The live `category` vocabulary drifted, because nothing validates it and each
extraction run improvised. Measured 2026-08-30 over 4,858 active facts:

```
project 1222   world 594   capability 509   note 754 (350 of them tasks)
identity 377   opinion/invariant 317   architecture 147   preference 105
```

Every one of those maps into **project** or **general**. Research, transcript
and fiction have no representation at all: zero facts, and one document in the
entire corpus.

That is the honest state of things. The taxonomy above is not a reorganization
of what exists, it is mostly a description of what is missing -- which is worth
saying plainly, because a schema that flattered the corpus would be useless for
deciding what to build.

The migration is therefore not urgent and not mechanical. Existing facts are
already almost all project or general; sorting them is a job for the
cluster-and-name pass in `wiki-organizer.md`, not a `UPDATE ... SET category`.

## Not decided here

- **Whether `category` becomes a closed vocabulary** enforced by the store, with
  everything else moving to `kind` and `subject`. Attractive, and it is a real
  migration over 4,858 rows, so it waits for the cluster pass.
- **How sensitivity is stored** -- a column on the fact, a property of the
  domain prefix, or derived from both. Deriving it from the domain is tempting
  and probably wrong: one restricted fact under an otherwise ambient domain
  should be expressible.
- **Whether business is ambient or restricted** while the team is two.
- **Transcript ingest volume**, per the note above. Measure first.
- **Where instructions live**, per the section above.
- **Everything in the conventions half**: what a subject is, the threshold at
  which a subject earns a synthesis page, the threshold at which one is split,
  and whether reassigning a subject supersedes or updates.
