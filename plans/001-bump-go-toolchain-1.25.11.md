# Plan 001: Bump the Go toolchain to 1.25.11 to clear two stdlib vulnerabilities

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report -- do not improvise. When done, update the status row for this plan
> in `plans/README.md` -- unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat b6a3d4f..HEAD -- go.mod go.sum`
> If go.mod changed since this plan was written, compare the "Current state"
> excerpt against the live file before proceeding; on a mismatch, treat it as
> a STOP condition.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: security
- **Planned at**: commit `b6a3d4f`, 2026-06-12

## Why this matters

`govulncheck` reports that the pinned Go 1.25.10 standard library carries two
vulnerabilities that this codebase actually calls, including GO-2026-5037 in
`crypto/x509` (reached via `http.Server.Serve` -> certificate verification in
`cmd/memstored/main.go:270`, and via `memstore.registerMCP` in
`cmd/memstore/setup_cmd.go:507`). Both are fixed in Go 1.25.11. memstored is
a TLS-serving network daemon, so x509 fixes are not optional hygiene. The
project's own Taskfile runs `govulncheck` as part of `task check`, so this
also restores a clean check run.

## Current state

- `go.mod:3` -- the toolchain directive:

  ```
  go 1.25.10
  ```

  There is no separate `toolchain` line; the `go` directive alone pins the
  version.
- `.github/workflows/ci.yml` uses `actions/setup-go@v6` with
  `go-version-file: go.mod`, so CI follows whatever go.mod says. No CI edit
  is needed.
- The Dockerfile floats a `golang:1.25` base image (commit 056c85c chose
  this deliberately so it satisfies the go.mod toolchain). No Dockerfile
  edit is needed.

## Commands you will need

| Purpose    | Command                                       | Expected on success |
|------------|-----------------------------------------------|---------------------|
| Build      | `GOWORK=off go build ./...`                   | exit 0              |
| Tests      | `GOWORK=off go test -race -count=1 ./...`     | all packages `ok` (pgstore skips without `MEMSTORE_TEST_PG`) |
| Vulncheck  | `GOWORK=off govulncheck ./...`                | no vulnerabilities affecting your code |
| Tidy       | `GOWORK=off go mod tidy`                      | exit 0, no diff beyond the version line |

Note: with the default `GOTOOLCHAIN=auto`, the go command downloads 1.25.11
automatically on first use. If the environment has `GOTOOLCHAIN=local` and an
older toolchain, that is a STOP condition.

## Scope

**In scope** (the only files you should modify):
- `go.mod` (the `go` directive only)
- `go.sum` (only if `go mod tidy` touches it)

**Out of scope** (do NOT touch):
- Any dependency version bumps -- dependabot owns those.
- Dockerfile, CI workflows -- both already track go.mod.

## Git workflow

- Branch off `main`: `advisor/001-bump-go-1.25.11`
- One commit. Message style matches the repo (short imperative, optional
  scope prefix), e.g.: `go.mod: bump toolchain to 1.25.11 for x509 fixes`
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Bump the go directive

Edit `go.mod:3` from `go 1.25.10` to `go 1.25.11`, then run
`GOWORK=off go mod tidy`.

**Verify**: `grep '^go ' go.mod` -> `go 1.25.11`; `git diff --stat` shows
only go.mod (and possibly go.sum).

### Step 2: Build and test

**Verify**: `GOWORK=off go build ./...` -> exit 0;
`GOWORK=off go test -race -count=1 ./...` -> all packages `ok`.

### Step 3: Confirm the vulnerabilities are gone

**Verify**: `GOWORK=off govulncheck ./...` -> output no longer lists
GO-2026-5037 or any "Your code is affected by N vulnerabilities from the Go
standard library" line. Informational findings about imported-but-uncalled
module vulnerabilities are acceptable.

## Test plan

No new tests -- this is a toolchain bump verified by the existing suite plus
govulncheck.

## Done criteria

- [ ] `grep '^go ' go.mod` prints `go 1.25.11`
- [ ] `GOWORK=off go test -race -count=1 ./...` exits 0
- [ ] `GOWORK=off govulncheck ./...` reports no stdlib vulnerabilities affecting this code
- [ ] No files outside go.mod/go.sum are modified (`git status`)
- [ ] `plans/README.md` status row updated

## STOP conditions

Stop and report back if:

- go.mod no longer says `go 1.25.10` (someone already bumped it -- verify
  govulncheck is clean and mark this plan DONE/REJECTED accordingly).
- The environment cannot download Go 1.25.11 (`GOTOOLCHAIN=local` or no
  network) -- report instead of pinning a different version.
- Tests fail after the bump -- report the failure output; do not patch code.

## Maintenance notes

- Patch-level toolchain bumps will recur; `task vulncheck` is the signal.
  Consider asking the maintainer whether dependabot's gomod ecosystem config
  should also track the toolchain directive.
