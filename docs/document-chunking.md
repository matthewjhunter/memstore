# Chunking documents: markdown

Status: design, not started
Author: Matthew + Claude
Date: 2026-07-19

Companion to `docs/document-corpus.md`, which settles what a chunk *is* -- a
verbatim span of a file, with provenance in typed columns -- and leaves open
where the cuts go. This covers markdown. Code chunking is a sibling document and
is not designed here.

## The constraint that shapes the design

Standard practice for markdown RAG is to prepend the heading breadcrumb to each
chunk's text, so a chunk under `## Vectors come later` carries
`Schema > Vectors come later` into the embedding and retrieves on those terms.

The verbatim invariant forbids it. The moment anything is prepended, the content
is no longer a byte-identical span of the file, and the property the whole corpus
design rests on is gone.

The resolution is that **embed text is not stored content**. The breadcrumb lives
in a derived column, and the embedding input is constructed at embed time from
breadcrumb plus content. That is not a workaround invented here -- it is the
existing labeled-prefix embed-text convention (tier 3 Phase 0), which already
specifies per-domain labels and already names `file` / `symbol` / `lang` as the
code-domain set. Documents use the same mechanism:

    path: docs/document-corpus.md
    section: Schema > Vectors come later
    <the verbatim span>

Stored content stays exactly what the file says. Retrieval sees the context.

One economy falls out of segmenting at headings: a section chunk starts *at* its
own heading line, so that heading is already inside the verbatim content.
`heading_path` therefore carries ancestors only, never the chunk's own heading.

## Sizing is in bytes, not tokens

Token counts are the usual unit and they are wrong here. A tokenizer is
model-specific, so token thresholds would put a model dependency inside the
ingest path -- violating the rule that nothing in ingest may reason -- and would
shift every boundary in the corpus when the embedding model changes. Chunking
must be stable across model changes. Bytes are.

    target   2000 bytes
    max      8000 bytes
    min       400 bytes

Target is what the splitter aims for; max is the hard ceiling above which a
section must split; min is the floor below which a section merges with its
siblings. A typical `##` section in these design docs lands as a single chunk,
which is the size that makes a hit precise without needing its neighbours to be
understood.

These are chosen for retrieval precision and injection cost, not for a context
window, because chunks ship FTS-only first and there is no embedder in the
picture to constrain them. Revisit when a model is selected -- and note that
revisiting means a re-ingest, since boundaries are baked into stored spans.

## The algorithm

1. **Front matter** (YAML or TOML) is parsed into document-level fields --
   title, tags, date -- and is not emitted as a chunk. It is structured metadata,
   not prose anyone wants returned from a search.

2. **Segment at headings.** A section runs from its heading line to the next
   heading of equal or higher level. Record `heading_path` (ancestors),
   `heading_level`, and `ordinal`.

3. **Split oversized sections at top-level block boundaries** -- paragraph, list,
   fenced block, table -- never inside one. A section over `max` splits at the
   last block boundary that fits.

4. **Merge undersized sections** with following siblings under the same parent
   heading, up to `target`. Stub sections that are only a heading and a sentence
   are common and retrieve badly alone.

5. **Atomic blocks are never split.** A fence or table larger than `max` becomes
   an oversized chunk and that is accepted. Half a code fence is syntactically
   meaningless and retrieves as noise; an oversized chunk is merely inefficient.

6. **No overlap.** Structural boundaries already put the cut where the meaning
   changes, so the sliding-window overlap that rescues fixed-size chunkers buys
   duplicate hits instead. The line-window fallback for non-markdown plain text
   keeps its overlap, since it has no structure to cut on.

### Fenced code inside markdown

A fence under `target` stays in its section chunk. Its meaning usually depends on
the prose introducing it, and divorcing a three-line example from the sentence
explaining it loses more than the cleaner separation gains.

A fence over `target` becomes its own chunk, with `lang` set from the info
string. Those are the ones that are genuinely standalone -- a full example
program, a schema block -- and setting `lang` makes them routable to the code
embedding space when it exists.

This is exactly the mixed case the routing doc's open question names: a chunk
holding prose and a small fence has claims on both spaces. It stays open here
too.

## Determinism

Re-ingesting an unchanged file must produce identical chunks. That is what makes
`file_sha256` a sufficient staleness check -- if the hash matches, no work is
needed, and that only holds if the chunker is a pure function of the bytes.

Which is why the document carries **`chunker_version`**. If the parser's AST
shifts across a version bump, an unchanged file re-chunks differently, and
without a recorded version there is no way to tell "the file changed" from "the
chunker changed." Bump it on any boundary-affecting change; a differing version
is grounds for re-ingest even when the hash matches.

## Parser

**goldmark.** CommonMark-correct, actively maintained, already the parser behind
the Hugo sites in this tree, and its AST nodes expose byte segments so exact
spans come for free rather than being reconstructed by counting.

A hand-rolled block scanner is tempting -- no dependency, no version drift, and
chunking needs far less than a full parser. It is the wrong trade. Setext
headings, indented code blocks, nested fences, and raw HTML blocks are where such
scanners break, and being wrong about a fence boundary corrupts a chunk in a way
that is invisible until someone reads a retrieval result and finds it truncated
mid-example.

Only the block parser is used. The renderer is never invoked, so the exposed
surface is the AST walk.

## Schema additions

On `memstore_document_chunks`:

    heading_path   text     -- ancestor headings, e.g. 'Schema > Vectors come later'
    heading_level  int
    lang           text     -- set on split-out fences; NULL otherwise

On `memstore_documents`:

    chunker_version  int    NOT NULL
    title            text   -- from front matter when present
    front_matter     jsonb  -- parsed; provenance metadata, not model-writable

`front_matter` sits in the rigid metadata system, not the flexible one: it is
derived from the file by the ingester, so it is subject to the same reserved-key
strip rule as every other provenance field.

## Test properties

Sharp enough to write before the implementation:

- Re-chunking an unchanged file is byte-identical, including ordinals.
- Every chunk's content equals `file[byte_start:byte_end]` exactly.
- Spans are non-overlapping and monotonically increasing in `ordinal`.
- No chunk boundary falls inside a fence or table. Good fuzz target.
- Sections between `min` and `max` emit as exactly one chunk.

Note what is *not* a property: chunks do not cover the file. Front matter and
inter-block whitespace are skipped, so "concatenating spans reconstructs the
original" is false by design. Non-overlap and monotonicity are the invariants;
total coverage is not one.

## Open questions

- **Merging across heading boundaries changes what `heading_path` means.** A
  chunk merged from two sibling stubs has two paths and stores the first. Storing
  the parent instead is more honest and less specific. Unresolved; the case is
  rare enough to defer until real corpora show how often it fires.
- **Thresholds are unvalidated.** 2000/8000/400 are reasoned, not measured. They
  want the same treatment the FTS tokenization work got -- a real corpus and a
  rank table -- once there is a corpus to measure.
- **Code chunking is not designed.** It is the sibling document, and it inherits
  the constraints here (verbatim, byte-sized, deterministic, versioned) while
  needing entirely different boundaries.
