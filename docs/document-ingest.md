# Ingest: the client command and the wire protocol

Status: design, settled 2026-07-19
Author: Matthew + Claude

Companion to `docs/document-corpus.md` (what a document is, the topology) and
the chunking designs. This settles how bytes actually reach the daemon: the CLI
command, the two HTTP endpoints, and the sync protocol that makes repo-scale
ingest incremental.

## Surface: a CLI command, not an MCP tool

`memstore ingest <path>`. A stat on the path decides file versus tree.

This stays off the MCP server, reaffirming the pillar in
`docs/document-corpus.md`: no MCP tool creates documents, and the model's only
document capability is search. "Claude, ingest this repo" still works -- Claude
runs the CLI through Bash like any other command -- but the distinction is not
ceremony. The CLI ingests files that exist on disk, verbatim, so the model
cannot launder prose into the corpus as a tool argument.

The residual risk is the model writing a file and then invoking the CLI on it.
The corpus already prices that in: a loose or dirty file lands untrusted or
marked by default, and untrusted chunks come back fenced. It is not new trust,
just a longer path to the same untrusted shelf.

### The ingest credential is its own token

`ingest` is implied by no other scope, so the CLI needs a token that carries it
-- and that token must be a *different credential* from anything the MCP server
reads, or the separation is nominal: memstore-mcp loads `MEMSTORE_API_KEY` from
the same config the CLI uses, and granting that shared token `ingest` would
hand the model's credential the exact power the scope split exists to withhold.

So: a dedicated config key (`ingest_token` / `MEMSTORE_INGEST_TOKEN`) that
`memstore ingest` reads and memstore-mcp never loads. Issued like any token,
scoped to exactly `ingest`:

    memstore admin issue-token --user matthew --scopes ingest matthew@laptop-ingest

## Repo mechanism: walk plus manifest sync

Considered against uploading a git bundle and against a `git archive` tar. The
bundle's one real advantage is that the daemon could *verify* repo provenance
from the object graph -- the only path to server-verified identity, worth
remembering if asserted provenance ever stops being enough. Everything else
favors the walk: bundles cannot be shallow, so a first ingest ships full
history; they carry committed objects only, so the dirty working tree -- which
the `dirty` flag exists to capture -- is structurally out of reach; loose files
need a second mechanism anyway; and unpacking hostile git objects is an attack
surface the daemon does not otherwise need. The walk covers every case with one
mechanism, and provenance stays client-asserted, which is the accepted threat
model.

The protocol, per repo:

1. **Enumerate** with `git ls-files` (tracked) plus
   `git ls-files --others --exclude-standard` (untracked but not ignored).
   Git's own ignore semantics do the filtering; the client writes no walker
   for the repo case. A file is `dirty` when `git status --porcelain` lists it
   -- modified or untracked -- which refines the earlier per-ingest flag into a
   per-file fact.
2. **Manifest**: `POST /v1/documents/sync` with the repo identity and one entry
   per file: `{path, file_sha256, size}`.
3. **Delta**: the daemon replies with `need` (new or changed, and routable --
   see below), `skip` (unroutable extension or over the size cap, with
   reasons), and `orphaned` (documents whose path is in the store but absent
   from the manifest: the file was deleted, so the document is deleted).
   Replace-on-`(repo, path)` handles changed files but can never notice
   deletions; the manifest is what closes that hole.
4. **Upload** each needed file: `POST /v1/documents` with the bytes and the
   asserted git metadata. The daemon hashes, verifies the hash against the
   manifest entry, chunks, stores.

First run uploads everything; every later run uploads only what changed. A
single-file ingest is step 4 alone, with the client walking up for `.git` to
derive repo identity, or `repo_url` NULL for a loose file.

Non-repo directories use a plain `filepath.WalkDir` in place of step 1 and are
otherwise identical: same manifest, same delta, `repo_url` NULL, paths relative
to the ingest root.

## The daemon decides what is ingestable

Type detection is by extension, and the extension table lives on the daemon,
not the client. The daemon owns the chunkers and `chunker_version`; a
client-side table would split that authority and rot independently. `.md` and
`.markdown` route to the markdown chunker, `.go` to the Go chunker, a curated
list of text extensions to the line-window fallback, and everything else is
skipped and reported. (`docs/document-corpus.md` said `lang` is set by the
ingesting caller; it moves daemon-side with the rest of the routing, same as
the chunking itself did.)

Because routing is decided at *manifest* time -- the daemon marks unroutable
paths `skip` in the sync response -- binaries and oversized files never cross
the wire at all. The client stays dumb; the authority that owns the table makes
the call before bytes move.

This also improves on a compromise recorded in `docs/code-chunking.md`:
`vendor/` skipping was client-side, with the noted cost that an exclusion the
daemon never sees is one it cannot verify. Under the manifest protocol the
daemon sees every enumerated path, so path-rule exclusions (`vendor/`, size
caps) move server-side and become policy the daemon applies rather than
behavior the client promises.

## Schema footnote: loose files break the uniqueness key

`UNIQUE (namespace, user_id, repo_url, path)` does not do what it says when
`repo_url` is NULL -- Postgres treats NULLs as distinct, so loose-file
re-ingest would accumulate rows instead of replacing. `NULLS NOT DISTINCT`
fixes it but requires Postgres 15, and the documented floor is 14. So: a
partial unique index on `(namespace, user_id, path) WHERE repo_url IS NULL`
alongside the main constraint. No version bump, same semantics.

## CLI report

Per run: files ingested (with chunk counts), skipped (grouped by reason),
orphans deleted, and the repo identity as asserted. Uploads run with small
bounded parallelism (implementation detail; start at 4). Exit nonzero if any
upload failed; skips are not failures.

## Remote sources: URLs

Settled 2026-08-30. `memstore ingest` today takes a filesystem path, and that
blocks the research category in `docs/schema.md` outright: research material
arrives as links, and a link that is read, applied, and never stored leaves the
decision it drove unfalsifiable.

**The converted markdown is the document.** Fetch, convert, chunk the markdown,
hash the markdown. The invariant runs against it exactly as against a file on
disk.

**The fetched bytes are not kept.** Storing the original HTML alongside was
considered and rejected as overhead without a use: it would double the storage
for every page to support an audit nobody performs, and the markdown is what
every consumer reads. The accepted consequence is that the conversion is not
re-derivable later and a page that changes or disappears cannot be diffed
against what we hold. That is a real loss and it is judged smaller than the
cost.

### PDFs are unsolved and matter

Web pages convert to markdown cleanly. PDFs do not, and a large share of
research papers -- the material this whole category exists for -- are PDFs.

The failure is worse than lossiness. Two-column academic layout makes text
extraction order ambiguous, so the "verbatim span" the corpus invariant depends
on may not correspond to anything a reader would recognise as a passage.
Figures, tables, ligatures and hyphenation all degrade further. A PDF pipeline
that produces plausible-looking but subtly reordered text would satisfy the
invariant while breaking what the invariant is for.

Marked here as a known gap rather than designed. URL ingestion ships for
HTML-to-markdown first; PDFs need their own decision about what the document is.

One part of that decision is settled: **PDFs keep their raw copy.** The rule is
not an arbitrary exception to the no-raw-copy decision above but the principle
underneath it made explicit -- *keep the original when the conversion cannot be
verified*. HTML to markdown is reliable enough that the markdown stands on its
own. PDF to text is not, so the original has to be retained as the thing a human
can check the extraction against. The same test decides future formats: scanned
images and ePub will land on the PDF side of it.

The consequence has to be said plainly rather than absorbed quietly. **PDF is
the first format where the corpus invariant cannot hold.** Chunks of extracted
text are not byte spans of the PDF, and no amount of care makes them so. The
honest response is to mark those chunks as derived rather than verbatim, and to
hash the PDF as the document, so that a consumer can tell the difference between
a chunk that is provably a span of its source and one that is a machine's
reading of it. Weakening the invariant everywhere to accommodate one format
would forfeit the guarantee for the formats that can actually keep it.

## Trust: the mechanical line

The `trusted` flag is where provenance is recorded, and it carries the whole
distinction:

| source | trusted |
|---|---|
| tracked, clean file in a repo the user owns, ingested by hand | yes |
| loose or dirty local file | no |
| any URL, even hand-fed | no |
| anything a model ingests, by any route | no |

Remote is untrusted regardless of who typed the command. A file in the user's
own repo has been reviewed by the act of committing it; a page on the internet
has not, whoever fetched it.

**This is a provenance record, not an access control, and it should not be
mistaken for one.** A model with shell access can invoke the CLI directly, so
the "only a human ingests trusted material" rule is not enforced against a model
that declines to follow it. Treating the flag as a security boundary would be
theater.

What it actually does is make the boundary *visible*: untrusted chunks come back
fenced, consumers can filter on it, and anything that arrived from outside is
identifiable as such forever. That is worth having on its own terms, and it is
consistent with this document's existing stance that provenance is
client-asserted under an accepted threat model.

The enforceable version, when it is wanted, is a web interface holding an
`ingest` token the model has no access to -- the same scope split this document
already describes, moved behind a surface the model cannot drive. Until then the
flag suffices.

## What this deliberately leaves out

- **No MCP ingest tool**, per above.
- **No bundle path.** Server-verified provenance is the one thing it buys;
  design it as an optional upgrade if and when asserted provenance stops being
  acceptable for some repo.
- **No daemon-side git.** The daemon never parses git objects; it sees bytes,
  paths, and assertions.
- **No content sniffing.** Extension routing only, unknown means skip. A
  misnamed file is skipped, reported, and can be renamed.
