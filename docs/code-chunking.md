# Chunking documents: Go source

Status: design, not started
Author: Matthew + Claude
Date: 2026-07-19

Sibling to `docs/document-chunking.md`, which covers markdown. Everything that
document establishes is inherited: chunks are verbatim spans, sized in
non-whitespace characters, produced deterministically, versioned via
`chunker_version`, and never overlapping. This covers where the cuts go in Go
source, and why the parser is `go/ast` rather than tree-sitter.

## Prior art

**cAST** (Findings of EMNLP 2025, arXiv 2506.15655) is the reference. Recursive
**split-then-merge** over the AST: walk top-down, recurse into a node's children
when it exceeds the budget rather than force-splitting it, then greedily merge
adjacent small siblings to raise information density. Measured against fixed-size
line chunking it gains +4.3 Recall@5 on RepoEval and +2.67 Pass@1 on SWE-bench.

That is the same shape the markdown design arrived at independently. Code gets
one thing markdown cannot have: an oversized node always has children to recurse
into, so "never force-split" is always satisfiable. Markdown's oversized fence is
a leaf, which is why that design has to accept oversized chunks.

`gomantics/chunkx` implements cAST in Go and is worth reading. It is not worth
depending on: it defaults to token counting and reaches tree-sitter through cgo.

### Where we diverge, deliberately

Every implementation surveyed **prepends context to chunk text** -- file path,
scope chain, signatures, imports. supermemory's `code-chunk` states the reason
exactly right: "Embedding models are trained on natural language. When you embed
`async getUser(id: string)`, the model doesn't inherently know this is inside a
UserService class."

The pressure is real and the conclusion is right; the implementation is not
available to us, because prepending destroys the verbatim invariant. The
resolution is the same one markdown reached: **the context is derived columns,
and the embedding input is assembled at embed time.** Same retrieval benefit, and
the stored content still matches the file byte for byte.

## Parser: go/ast, not tree-sitter

|  | cgo | coverage |
|---|---|---|
| `go/ast` (stdlib) | none | Go only, exact |
| `tree-sitter/go-tree-sitter`, `smacker/go-tree-sitter` | yes | 30+ languages |
| `malivvan/tree-sitter` (wazero/WASM) | no | 30+ languages |
| `odvcencio/gotreesitter` (pure-Go runtime) | no | ~205 grammars |

Go first, with the stdlib parser. The tree this corpus serves is ~93 Go modules,
so Go alone yields a corpus large enough to run the embedder bake-off, with no
dependency and no cgo in the ingest path. `go/ast` also beats a generic grammar
on fidelity for the things that matter here: it attaches doc comments to their
declarations, distinguishes methods by receiver, and knows what is exported.

Other languages come later via a cgo-free tree-sitter, with the line-window
fallback covering the rest in the meantime. Choosing between the wazero wrapper
and the pure-Go runtime is deferred until there is a second language to ingest.

**The cost, stated plainly:** a Go-only first corpus biases the embedder bake-off
toward Go. The candidate code models are trained on multi-language corpora, and a
Go-only evaluation may not rank them the way a mixed corpus would. Accept it
deliberately, and re-run the bake-off when a second language lands.

## Boundaries

**The unit is the top-level declaration.** A `FuncDecl` (function or method), or
a `GenDecl` (a `type`, `const`, `var`, or `import` block).

**A declaration's doc comment is part of its chunk.** The span starts at
`Doc.Pos()`, not `Decl.Pos()`. In idiomatic Go the doc comment is the most
retrievable prose in the file -- it says in English what the code does -- and
separating it from its declaration would put the question and the answer in
different chunks. `go/ast` attaches it for free; a generic grammar makes you
reconstruct the association from adjacency.

Then, following cAST:

1. **Oversized declarations recurse into children.** A long function splits at
   top-level statement boundaries inside its body; a large `const` or `var` block
   splits at spec boundaries. Never split inside a statement or a composite
   literal.
2. **Undersized declarations merge with adjacent siblings** up to `target`. A
   file of one-line accessors becomes a few chunks, not forty.
3. **The package clause and imports** form the file's header chunk together with
   the package doc comment when there is one. For a `doc.go` that is nothing but
   package documentation, this is the whole file and it is prose -- which puts it
   squarely in the mixed-space open question from
   `docs/embedding-model-routing.md`.

## Derived columns

Assembled into embed text; never prepended to stored content.

    package        package name
    import_path    resolved module-relative import path
    symbol         declaration name
    receiver       receiver type for methods, NULL otherwise
    decl_kind      func | method | type | const | var | import | package_doc
    exported       bool
    signature      the declaration's signature line
    scope_path     package > receiver > symbol
    imports_used   imports actually referenced within the span

`imports_used` is lifted directly from `code-chunk`, which prepends up to ten
referenced import symbols and reports it as one of the higher-value context
fields. It is a genuine signal about what a function *does* -- a function
touching `crypto/subtle` and one touching `net/http` are different in a way the
body alone may not make obvious to an embedding model.

Embed text then assembles as:

    path: pgstore/store.go
    package: pgstore
    symbol: (*PostgresStore).appendUserFilter
    signature: func (s *PostgresStore) appendUserFilter(b *strings.Builder, col string)
    <the verbatim span>

## What not to ingest

Corpus quality is decided as much by exclusion as by chunking:

- **`vendor/`** -- skipped entirely. Third-party source, already covered by the
  repo trust rules, and it would swamp the corpus by volume.
- **Generated files** -- detected by the standard `^// Code generated .* DO NOT
  EDIT\.$` header. Ingested but marked `generated`, and excluded from retrieval
  by default. Protobuf and mock output is high-volume and low-value, and left
  unmarked it would dominate results for any type name it mentions.
- **`_test.go` files** -- ingested and marked `is_test`. Tests document intended
  behavior and are often the clearest statement of what a function is for, so
  excluding them loses real signal; marking them lets retrieval choose.

Both flags belong to the rigid provenance metadata rather than the flexible
kind, and both are computed rather than declared: `generated` from the file's
own bytes, which the daemon has, and `is_test` from the filename. Neither is a
field a model can set.

Note the split from `docs/document-corpus.md`: skipping `vendor/` happens
client-side, because the client is what walks the tree and decides what to
upload. An exclusion the daemon never sees is an exclusion the daemon cannot
verify -- acceptable for volume control, but it means "no vendored code in the
corpus" is a property of the ingest client's behavior, not an invariant.

## Unparseable files

`go/parser` fails on a syntax error, or on a file using language features newer
than the toolchain doing the ingest. That is not exceptional -- it will happen on
work in progress and on repos pinned to newer Go versions.

Such a file falls back to line-window chunking and is marked with its chunking
strategy on the document. Falling back is right: a file that does not parse still
contains searchable text, and refusing to ingest it creates a silent hole in the
corpus. Recording the fallback matters so it is visible, and so re-ingest can
retry once the toolchain catches up.

## Test properties

Inherits the markdown battery -- byte-identical re-chunking, exact span
equality, non-overlap, monotonic ordinals. Go-specific additions:

- A doc comment is always in the same chunk as the declaration it documents.
- A chunk that holds whole top-level declarations re-parses as a valid
  declaration list. (Statement-level chunks from an oversized function body do
  not, and are exempt.)
- No chunk boundary falls inside a string literal, composite literal, or
  statement. Fuzz target.
- Ingesting this repository produces zero unparseable-fallback documents.

## Evaluation

The research turned up the eval design that `docs/document-chunking.md` says is
missing. supermemory found their first benchmark was trivially saturated: queries
drawn as code prefixes from the target file hit 100% recall and measured nothing.
Their fix is worth copying wholesale:

- **Hard negatives** -- ~500 distractor files drawn from the *same repository*,
  so retrieval has to discriminate within a codebase rather than between
  codebases.
- **Intersection-over-Union threshold (0.3)** against the target span, which
  penalizes bloated chunks that technically contain the answer while burying it.

That IoU metric is the missing instrument. Chunk boundary quality was flagged as
the failure mode with no visible symptom; IoU gives it a number, and makes the
thresholds in `document-chunking.md` measurable rather than merely reasoned.

## Open questions

- **Bake-off bias from a Go-only corpus.** Stated above; the mitigation is to
  re-run once a second language is ingested.
- **`imports_used` costs resolution work** the rest of the ingest path does not
  need. Within a file it is a scope walk; across files it needs the import block,
  which the header chunk already parses. Whether it is worth doing for every
  chunk or only for function-level ones is unmeasured.
- **Where package documentation goes.** A `doc.go` is prose in a `.go` file, so
  the text/code space routing question applies within a single document rather
  than between documents.
