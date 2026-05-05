# FIX-TODO — Test-quality sweep (gates dropping the `_test.go` exclusion for default linters)

> Carve-out from the architectural TODOs that survived the
> April 2026 sprint. Companion files: `FIX-004.md`, `FIX-006.md`,
> `FIX-TODO-clock-injection.md`, `FIX-TODO-error-wrapping.md`,
> `FIX-TODO-return-types.md`.

## Why this exists

`golangci-lint`'s default linter set (`errcheck`, `staticcheck`,
`ineffassign`, `unused`, `govet`) is **excluded from `_test.go`
files** today via this rule in `.golangci.yml`:

```yaml
- linters: [errcheck, staticcheck, ineffassign, unused]
  path: "_test\\.go$"
```

The exclusion was added during FIX-005 because the test code
contains pre-existing issues that would block the pipeline:
unchecked `defer Close()` errors, single-case `select` blocks,
empty `if errors.Is` branches, lifted loop conditions, etc.

Sweeping these issues is genuinely useful — they catch real bugs
in test fixtures that mask production failures. But it's a separate
focused task from the architectural FIX-* sweep.

## Current state (snapshot at FIX-009)

Counted by adapter at the time the exclusion was added:

```text
adapters/mqtt/transport/paho        — 11 issues
adapters/aws/store/dynamodb*        —  ~5
adapters/native/credentials/file    —  1
adapters/native/store/sqlite*       —  ~3
testutil/{asblocal,localstack,
          rabbitmqlocal,s3local}    — ~10 (errcheck on Close)
runtime/                            —  ~5 errcheck
config/                             —  ~3
httpapi/                            —  ~5
```

The numbers are approximate (the exact list is regenerable by
removing the exclusion and running `make lint-go`).

Common failure patterns in tests:

1. `defer sess.Close()` — errcheck wants the error checked or
   explicitly discarded. Fix: `defer func() { _ = sess.Close() }()`.
2. `select { case <-ch: }` — staticcheck S1000 wants
   `<-ch` directly. Fix: replace with the bare receive.
3. `if errors.Is(err, X) { /* empty */ }` — staticcheck SA9003.
   Fix: remove the dead branch or add an actual handler.
4. `for rows.Next() { ... if err := rows.Scan(...); err != nil { rows.Close(); return ... } }`
   — staticcheck QF1006 wants the close lifted into the loop's exit.
5. Unused test helper: `unused`. Fix: delete or use.

## Approach

### Phase 1 — Decide the scope unit

Two approaches:

- **All-at-once**: drop the exclusion, fix every violation in one
  sweep, commit a single "test-quality cleanup" PR.
- **Per-package**: drop the exclusion for one path at a time,
  fix that package's violations, commit, repeat.

Per-package is safer: each commit is reviewable and reverts cleanly
if a "fix" breaks the test's intent.

### Phase 2 — Per-package fix template

For each package:

1. Remove the path from the exclusion (or scope the exclusion
   tighter):
   ```yaml
   - linters: [errcheck, staticcheck, ineffassign, unused]
     path: "_test\\.go$"
     # path-except not yet supported in golangci-lint v2; alternative:
     # use multiple narrower exclusions, e.g. specific subpaths only.
   ```
   Easier alternative: keep the exclusion in place, and instead use
   a `nolint:errcheck` directive on the lines you can't yet fix —
   but the goal is to FIX, not silence.
2. Run `golangci-lint run ./<pkg>/...` to enumerate.
3. Fix each issue in the smallest reasonable change. Common patterns:
   - `defer x.Close()` → `defer func() { _ = x.Close() }()`
   - `x.Close()` (where x is *sql.DB / io.Closer) → `_ = x.Close()`
   - `select { case x := <-ch: ... }` (single case) → `x := <-ch; ...`
   - Empty error branch → delete or add real handler.
4. Run `go test -race ./<pkg>/...` to verify behaviour preserved.
5. Commit `FIX-TODO-test-quality: clean up <pkg>`.

### Phase 3 — When all packages pass

Drop the exclusion entirely from `.golangci.yml`:

```yaml
exclusions:
  rules:
    # (this block deleted)
    # - linters: [errcheck, staticcheck, ineffassign, unused]
    #   path: "_test\\.go$"
```

`make lint` passes. Future test code that introduces these issues
fails the gate.

## Cost estimate

- ≈ 50–80 violation sites across all packages.
- Each fix is < 5 lines.
- Total: **1–2 dedicated days** (most time spent reading test
  context to make the right judgement, not editing).

## Risks

- **Hiding a real test bug.** `errors.Is` with empty branch may
  have been a TODO marker for a real handler that was never
  written. Read each context before deleting.
- **Test rendered useless.** Replacing a single-case select with a
  bare receive removes the cancellability semantics if the test
  ever needed them. Verify the test's intent first.
- **Flake amplification.** Some "lifted close" patterns were
  defensive — restructuring may surface a race that was previously
  benign. Run with `-race -count=10` before commit.

## Acceptance

- The `_test.go` exclusion for default linters is removed from
  `.golangci.yml`.
- `make lint` passes across all modules.
- A trial unchecked `defer Close()` in a test file fails the gate.
- Test suite green with `-race -count=3` (smoke for new flakes).

## Related

- Original plan: `FINAL_DDD_HEX_CLEAN_FIX_PLAN.md` § FIX-005.
- Sibling carve-outs: `FIX-TODO-clock-injection.md`,
  `FIX-TODO-error-wrapping.md`, `FIX-TODO-return-types.md`.
- The exclusion lives in `.golangci.yml` near the bottom of
  `exclusions.rules`.

## Progress (work-tasklist)

- **T001 — Clean up adapters/mqtt/transport/paho test-quality (~11 issues) — DONE (2026-05-05)**
  Cleaned up 11 test-quality lints in `adapters/mqtt/transport/paho`:
  errcheck on `defer Close()` / `t.Cleanup(... Close(...))` wrapped as
  `_ = ...`; QF1006 lifted-loop close in
  `TestAnaIntg_MultipleReceivers_SameTopic_AllReceive` rewritten via DeMorgan
  (preserves inner deadline/tick semantics); two single-case `select { case
  <-time.After(d): }` rewrites to bare `<-time.After(d)` (no `ctx`/`done`
  arms — semantically identical); SA9003 empty `errors.Is(err,
  context.Canceled) {}` removed (its documentation comment preserved as a
  top-level comment); orphaned `errors` import dropped; one-line bump in
  `audit/test-timing-allowlist.txt` (241 → 240) tracking the removed import.
  `make lint` and `make test` green. Reviewed by `thiink-test-reviewer`
  (APPROVED first pass); codex unavailable.
- **T002 — Clean up adapters/aws/store/dynamodb* test-quality (~5 issues) — DONE (2026-05-05)**
  Verified clean: ran `golangci-lint run` against
  `adapters/aws/store/dynamodblease`, `adapters/aws/store/dynamodbdlq`,
  `adapters/aws/store/dynamodboutbox`, and `adapters/aws/store`
  (containing `factory_test.go`) with the `_test.go` exclusion for
  default linters (`errcheck`, `staticcheck`, `ineffassign`, `unused`)
  temporarily removed — 0 issues across all four packages. The
  ~5-issue snapshot in "Current state" was taken before subsequent
  FIX-* sweeps (wrapcheck/ireturn cleanups) incidentally cleared the
  dynamodb test files. `go test -race -count=1 ./...` green for all
  three modules. No code changes were required for this task; only
  this progress note. Phase 3 (T009) will drop the exclusion
  globally and re-confirm.
- **T003 — Clean up adapters/native/credentials/file test-quality (~1 issue) — DONE (2026-05-05)**
  Verified clean: ran `golangci-lint run ./...` against
  `adapters/native/credentials/file` with the `_test.go` exclusion for
  default linters (`errcheck`, `staticcheck`, `ineffassign`, `unused`)
  temporarily removed — 0 issues. The ~1-issue snapshot in "Current
  state" was taken before subsequent FIX-* sweeps (wrapcheck/ireturn
  cleanups) incidentally cleared the file. `go test -race -count=1
  ./...` green. No code changes were required for this task; only
  this progress note. Phase 3 (T009) will drop the exclusion globally
  and re-confirm.
- **T004 — Clean up adapters/native/store/sqlite* test-quality (~3 issues) — DONE (2026-05-05)**
  Cleaned up errcheck violations in
  `adapters/native/store/sqliteoutbox/store_test.go` and
  `adapters/native/store/sqlitedlq/store_test.go`. Targeted
  `golangci-lint run --no-config
  --enable=errcheck,staticcheck,ineffassign,unused --tests` against
  both packages reports 0 issues — the actual count was 10 unchecked
  `Close()`/`Write()` calls (vs the ~3 snapshot in "Current state",
  taken before later test additions). Used `_ = s.Close()` for
  housekeeping `t.Cleanup`/`defer` paths, explicit `t.Fatalf` on
  semantically meaningful close points (close-before-reopen,
  close-then-stat) where a silent close failure could mask flush /
  persistence bugs, and a small `mustWrite(t, s, ctx, entry)` helper
  in the dlq test file for the seed-data sites. No production code
  touched, `.golangci.yml` unchanged, no new tests.
  `go test -race -count=1 ./...` green for both packages.
  Phase 3 (T009) will drop the exclusion globally and re-confirm.
- **T005 — Clean up testutil/{asblocal,localstack,rabbitmqlocal,s3local} test-quality (~10 issues) — DONE (2026-05-05)**
  Verified clean: each of the four `testutil/*` submodules is its own
  Go module, so lint was run from inside each module with
  `golangci-lint run --no-config
  --enable=errcheck,staticcheck,ineffassign,unused ./...` — 0 issues
  across all four. `go build ./...` and `go vet ./...` green for each
  module. The ~10-issue snapshot in "Current state" was taken before
  subsequent FIX-* sweeps (wrapcheck / ireturn / err-naming cleanups)
  incidentally cleared the violations. These packages contain no
  `_test.go` files (they are docker-lifecycle helpers consumed by
  other modules' tests), so the `_test.go` exclusion in
  `.golangci.yml` does not apply here — the helper code itself was
  already subject to the default linters and is clean. No code
  changes were required for this task; only this progress note.
  Phase 3 (T009) will drop the `_test.go` exclusion globally and
  re-confirm across the consuming test suites.
- T006 — Clean up runtime/ test-quality (~5 errcheck) — pending
- T007 — Clean up config/ test-quality (~3 issues) — pending
- T008 — Clean up httpapi/ test-quality (~5 issues) — pending
- T009 — Phase 3: drop the _test.go exclusion from .golangci.yml and verify make lint green — pending
