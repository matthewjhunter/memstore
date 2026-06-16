# Plan 002: Enforce a request body size limit on every httpapi endpoint

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md` -- unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat b6a3d4f..HEAD -- httpapi/handler.go httpapi/handler_test.go`
> If either file changed since this plan was written, compare the "Current
> state" excerpts against the live code before proceeding; on a mismatch,
> treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: security
- **Planned at**: commit `b6a3d4f`, 2026-06-12

## Why this matters

memstored is a network-exposed daemon, and no endpoint bounds the request
body. `readJSON` decodes `r.Body` directly, so a client (or a compromised
token holder) can stream a multi-gigabyte JSON body and drive the daemon to
OOM. The server sets `ReadTimeout`/`ReadHeaderTimeout`/`WriteTimeout`
(`cmd/memstored/main.go:224-230`) and `MaxHeaderBytes` has a sane 1 MB
default, but the body is the unbounded path. One cap applied at the
`ServeHTTP` chokepoint closes it for every route at once.

## Current state

- `httpapi/handler.go:101-113` -- `New()` constructs the Handler, applies
  functional options (`HandlerOpt`, see `WithTokenVerifier` at line 95 for
  the option pattern to copy), then calls `h.registerRoutes()`.
- `httpapi/handler.go:115-153` -- `ServeHTTP` is the single entry point: it
  short-circuits `/v1/health`, performs bearer auth, then dispatches to
  `h.mux`. This is where the body wrap belongs.
- `httpapi/handler.go:667-674` -- the shared decode helper:

  ```go
  func readJSON(r *http.Request, w http.ResponseWriter, v any) bool {
      if err := json.NewDecoder(r.Body).Decode(v); err != nil {
          writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
          return false
      }
      return true
  }
  ```

- Legitimate large bodies exist: `POST /v1/sessions/transcript`
  (`httpapi/session_archive.go:33`) receives an entire session transcript as
  a JSON string field (`Content`), which can be tens of megabytes. The
  default cap must clear that comfortably -- use 64 MB.
- Error responses go through `writeError(w, status, msg)` (same file);
  match that convention.
- `httpapi/handler_test.go:1-60` -- test exemplar: `newTestHandler(t)`
  builds a Handler over an in-memory SQLite store with a `mockEmbedder`;
  tests drive it with `httptest` requests.

## Commands you will need

| Purpose | Command                                                  | Expected on success |
|---------|----------------------------------------------------------|---------------------|
| Build   | `GOWORK=off go build ./...`                               | exit 0              |
| Tests   | `GOWORK=off go test -race -count=1 ./httpapi/`            | ok                  |
| All     | `GOWORK=off go test -race -count=1 ./...`                 | all packages ok     |
| Vet     | `GOWORK=off go vet ./...`                                 | exit 0              |
| Format  | `test -z "$(gofmt -l .)"`                                 | exit 0              |

## Scope

**In scope** (the only files you should modify):
- `httpapi/handler.go`
- `httpapi/handler_test.go`

**Out of scope** (do NOT touch, even though they look related):
- `cmd/memstored/main.go` -- exposing the limit as a daemon flag is a
  follow-up, not this plan. The HandlerOpt default suffices.
- Per-route limits, `MaxHeaderBytes`, `IdleTimeout` -- considered and
  deliberately excluded (Go defaults/fallbacks already cover them).
- `httpapi/session_archive.go` -- no change; the 64 MB default accommodates
  transcripts.

## Git workflow

- Branch off `main`: `advisor/002-body-size-limits`
- One or two commits, e.g.: `httpapi: cap request body size at 64MB`
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Add the cap field and option

In `httpapi/handler.go`:
- Add a field to the `Handler` struct: `maxBodyBytes int64`.
- In `New()`, default it before applying options: `maxBodyBytes: 64 << 20`.
- Add a functional option following the `WithTokenVerifier` pattern:

  ```go
  // WithMaxBodyBytes caps the request body size accepted by any endpoint.
  func WithMaxBodyBytes(n int64) HandlerOpt {
      return func(h *Handler) { h.maxBodyBytes = n }
  }
  ```

**Verify**: `GOWORK=off go build ./httpapi/` -> exit 0

### Step 2: Wrap the body in ServeHTTP

In `ServeHTTP`, immediately before the final `h.mux.ServeHTTP(w, r)` (after
auth, so unauthenticated requests are rejected without touching the body):

```go
if r.Body != nil {
    r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
}
```

Also apply it on the `/v1/health` early-return path or skip it there --
health is a GET with no body; skipping is fine. Keep the change minimal.

**Verify**: `GOWORK=off go test -race -count=1 ./httpapi/` -> ok (existing
tests pass; nothing legitimate exceeds 64 MB).

### Step 3: Return 413 instead of 400 when the cap trips

`http.MaxBytesReader` makes the decoder fail with `*http.MaxBytesError`.
Update `readJSON`:

```go
func readJSON(r *http.Request, w http.ResponseWriter, v any) bool {
    if err := json.NewDecoder(r.Body).Decode(v); err != nil {
        var maxErr *http.MaxBytesError
        if errors.As(err, &maxErr) {
            writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
            return false
        }
        writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
        return false
    }
    return true
}
```

Add `"errors"` to the imports if absent.

**Verify**: `GOWORK=off go build ./httpapi/` -> exit 0

### Step 4: Tests

See test plan.

**Verify**: `GOWORK=off go test -race -count=1 ./httpapi/ -run TestBodyLimit` -> ok

## Test plan

In `httpapi/handler_test.go`, modeled on the existing `newTestHandler` +
`httptest` style:

- `TestBodyLimit_OversizedRequestRejected`: build a handler with
  `httpapi.WithMaxBodyBytes(1024)` (construct via `httpapi.New` directly,
  mirroring `newTestHandler`'s internals, since `newTestHandler` does not
  pass options -- either extend it variadically or build inline). POST a
  >1024-byte JSON body to `/v1/facts`. Expect status 413.
- `TestBodyLimit_NormalRequestPasses`: same handler, small valid insert
  body -> 200/201 as the existing insert tests expect.

Run: `GOWORK=off go test -race -count=1 ./httpapi/` -> all pass including
the 2 new tests.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `GOWORK=off go build ./...` exits 0
- [ ] `GOWORK=off go test -race -count=1 ./...` exits 0; the two new TestBodyLimit tests exist and pass
- [ ] `grep -n "MaxBytesReader" httpapi/handler.go` returns exactly one match
- [ ] `test -z "$(gofmt -l .)"` exits 0
- [ ] No files outside the in-scope list are modified (`git status`)
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back (do not improvise) if:

- `ServeHTTP` or `readJSON` no longer match the "Current state" excerpts.
- An existing test fails because a legitimate request exceeds 64 MB -- that
  means the default is wrong; report rather than silently raising it.
- The fix appears to require touching `cmd/memstored/main.go`.

## Maintenance notes

- If transcript sizes ever approach 64 MB in practice, the cap needs a
  daemon flag (`--max-body-bytes`) wired through `cmd/memstored/main.go`;
  that was deliberately deferred.
- Reviewers should confirm the wrap happens after auth (an attacker without
  a token should never reach body handling) and that 413, not 400, is
  returned -- clients use the distinction.
