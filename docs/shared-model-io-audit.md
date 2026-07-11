# Audit: is a shared MCP / model-I/O module justified?

**Status:** decision recorded and executed for memstore. Step 1 landed the fence
in `internal/fence` (#99, 2026-07-10). The shared module now exists --
[`github.com/matthewjhunter/airlock`](https://github.com/matthewjhunter/airlock)
(public, Apache-2.0, v0.0.1), packages `wrap` (fence) and `unwrap` (llm-json).
memstore now consumes it: `internal/fence` is deleted and the three lenient-JSON
parsers route through `unwrap`. Steps 2 and 3 are done **for memstore**; herald
and image have not yet migrated. The open question below is resolved -- the name
is `airlock`.
**Scope of audit:** every repo touching `github.com/modelcontextprotocol/go-sdk`,
plus the model-plumbing code that isn't MCP at all.
**Prompted by:** the [`prompt-fencing`](prompt-fencing-internal-llm.md) fix
raising "don't hand-roll a third copy of the fence."

## Question

Two shapes were on the table: an `mcp-utils` module for shared MCP-server
helpers, or a single-purpose fence module. Before creating either, audit what is
actually duplicated.

## Landscape

Five repos import the official go-sdk:

| Repo | Version | Role |
|---|---|---|
| `matthewjhunter/math-mcp` | v1.6.0 | server |
| `matthewjhunter/memstore` | v1.6.1 | server |
| `matthewjhunter/majordomo` | v1.6.0 | server |
| `old-school-gamers/dice` | v1.5.0 | server |
| `matthewjhunter/image` | v1.5.0 | **client** (`mcp.NewClient` + `CallTool`) |

Herald has **no** MCP SDK dependency. It consumes the fence in its own AI
pipeline. That single fact settles the category question: the fence is not an MCP
concern, because its most invested consumer never speaks MCP.

## What is actually duplicated

### `textResult` -- real, verbatim, cross-org, but deprecated

Identical function in two repos:

- `old-school-gamers/dice` -- `cmd/dice-mcp/main.go:112`
- `matthewjhunter/memstore` -- `mcpserver/server.go:1905`

```go
func textResult(text string, isError bool) *mcp.CallToolResult {
    return &mcp.CallToolResult{
        Content: []mcp.Content{&mcp.TextContent{Text: text}},
        IsError: isError,
    }
}
```

`matthewjhunter/majordomo` has the same construction **inlined five times**
(`internal/mcp/server.go:135, 151, 160, 172, 189`), never factored into a helper.

The decisive observation: **`math-mcp` does not have this pattern at all.** It
returns typed output (`FinResult` via an `envelope()` helper, `financial.go:150`)
and lets the SDK derive the schema and populate `StructuredContent`. It already
lives where [`structured-tool-output.md`](structured-tool-output.md) is trying to
move memstore. So `textResult` is not a primitive worth sharing -- it is the
prose-blob pattern the structured-output migration removes. A shared module
seeded with `textResult` would enshrine the anti-pattern.

**Resolution: delete, don't share.** Migrate dice and majordomo to typed `Out`
like math-mcp, and the duplication evaporates instead of acquiring a home.

### Server bootstrap -- not worth abstracting

`mcp.NewServer(&mcp.Implementation{...}, opts)` + `server.Run(ctx,
&mcp.StdioTransport{})` is near-identical across all four servers, but it is 3-4
lines of SDK calls and the options genuinely diverge: math-mcp sets
`ServerOptions.Instructions`, memstore and dice pass `nil`, majordomo threads a
`socketPath`. Wrapping it hides the one line per repo (`Instructions`) that should
be visible. Leave it.

### Lenient LLM-JSON extraction -- real, and NOT MCP

Strip markdown fences, pull the first balanced object from model output. Present
in:

- memstore -- `curator.go`, `extract.go`, `httpapi/extractqueue.go`,
  `httpapi/session_archive.go`, `internal/conformance/conformance.go`
- herald -- `extract.go`
- `image` -- the client-side completion path

This spans the same repos as the fence and is the same category: handling model
text safely. It is not MCP.

## Decision

**Do not build `mcp-utils`.** The only MCP-specific duplication is an 8-line
helper for a pattern being deprecated; that does not justify a vendored,
tag-and-vendor cross-org dependency at four consumers. If a genuine MCP-helper
cluster appears later (structured-result test helpers, a `resultStructured[T]`
assertion, registration patterns), name it then, from evidence -- not
speculatively, and not as a `-utils` grab bag.

**Do build a shared model-I/O-hygiene module** (name unsettled -- see below). It
holds two coherent pieces under one sentence, "handle untrusted or model-authored
text safely on the way into and out of a prompt":

- **fence** (shipped as package `wrap`) -- `Nonce()`, `Neutralize(string)`,
  `Untrusted(nonce, string)` (renamed from `Wrap` to avoid `wrap.Wrap` stutter).
  The spotlighting primitive. Consumers: memstore (done), herald, a future harness.
- **llm-json** (shipped as package `unwrap`) -- the lenient, fence-tolerant JSON
  extraction, generalized with a string-aware scanner (`JSON`, `Into[T]`).
  Consumers: memstore (done), herald, image.

This reuses far more broadly than an MCP-result helper, because it is not tied to
a transport. It is security-relevant, which is the same argument for
single-source-plus-tag-propagation already made for the markdown sanitizer in the
[portfolio module policy](../SECURITY.md) (private module, consumed by
`go mod vendor`, fix propagates via `git tag`).

### Sequencing

1. ~~Land the memstore fence fix as an `internal/` package first~~ -- done (#99).
2. ~~Extract `fence` to the shared module~~ -- done: created `airlock`, package
   `wrap`; memstore migrated and `internal/fence` deleted. Herald still to migrate.
3. ~~Fold `llm-json` in when you next touch that code~~ -- done: package `unwrap`
   sits cleanly beside `wrap`; memstore's three parsers route through it.
4. Still open: migrate dice and majordomo to typed `Out` (reference:
   math-mcp `internal/financial/financial.go` `envelope()`), which retires the
   `textResult` duplication without a module. Also still open: migrate herald and
   image onto `airlock`.

## Resolved -- the repo name

Settled as **`airlock`**: a concept name (a chamber you pass through between
incompatible environments -- trusted process on one side, untrusted model on the
other), not a layer name, and it survives holding both directions. It beat
`modelio` (a layer name, junk-drawer risk) and `fence` (stutters with the package
name). `-utils`/`mcp-utils` were the smell this audit rejected.

## Audit caveats

- Findings are from function signatures, helper definitions, and go.mod SDK
  pins -- not a line-by-line read of every handler body. "math-mcp never builds a
  text result" is inferred from its uniform `return nil, FinResult{}, err` return
  pattern; strong, but confirm against `financial.go` before using it as the
  migration reference.
- SDK versions span v1.5.0-v1.6.1. Typed-`Out` schema derivation and
  `StructuredContent` population are verified present in v1.6.1 (memstore's pin);
  confirm the same API on v1.5.0 before migrating dice.
