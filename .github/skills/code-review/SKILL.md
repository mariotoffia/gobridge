---
name: code-review
description: Review GoBridge pull requests and diffs. Use when asked to review a PR, a branch, or staged changes in this repository. Covers what to check, what lint already enforces, what not to suggest, and which path-scoped instruction files hold the rules for each area.
---

# Reviewing a GoBridge change

GoBridge is a Go multi-module message bridge built on Hexagonal + DDD + Clean
Architecture. `make lint` and `make test` already enforce most style and
layering rules. A review is worth reading only when it finds what they cannot:
wrong behaviour, a broken contract, a missing test, or docs that promise more
than the code does.

## Step 0: load the rules for the touched paths

Read the instruction files that match the diff before you review it. They hold
the rules for each area.

| Changed path | Read |
|---|---|
| `adapters/**` | `.github/instructions/adapters.instructions.md`, then the transport file below |
| `adapters/mqtt/**` | `mqtt.instructions.md` |
| `adapters/aws/transport/**` | `sqs.instructions.md` |
| `adapters/azure/**` | `servicebus.instructions.md` |
| `adapters/amqp/**` | `amqp.instructions.md` |
| `adapters/http/**` | `http-transport.instructions.md` |
| store, lease, rollout and config-store adapters, `ports/storetest` | `stores.instructions.md` |
| `runtime/**`, `bridge/**`, cluster adapters | `runtime.instructions.md` |
| `config/**`, `validate/**`, plugin config, content identity | `config.instructions.md` |
| `httpapi/**`, `spec/httpapi/**` | `httpapi.instructions.md` |
| `processors/**`, `circuitbreaker/**` | `processors.instructions.md` |
| `domain/**`, `ports/**` | `domain-ports.instructions.md` |
| `*_test.go`, `tests/**`, `testutil/**` | `tests.instructions.md` |
| `scripts/release/**`, `.github/workflows/**`, `Makefile` | `release.instructions.md` |

## Step 1: understand the intent

- Read the PR description and any linked issue. Check that the change does what
  the issue asked, and nothing it ruled out. A decision recorded in an issue or
  an ADR is not reopened in review.
- Read the whole changed file and the callers of any changed function, not only
  the hunk. Many real bugs here were in code next to the diff.
- When a bug is fixed in one adapter, check the sibling adapters for the same
  bug (MQTT, SQS, Service Bus, AMQP 0-9-1, AMQP 1.0, HTTP).

## Step 2: what lint already enforces — do not flag

Naming, layering (`.go-arch-lint.yml`), plugin-config shape, the ACL boundary,
`time.Now` / `time.Sleep` in production code, registry symmetry, planning
identifiers in Go source (tests included), gofmt, go vet and golangci-lint rules. If
one of these is broken, CI is red; at most name the checker (see `LINT.md`).

Build tags, compile errors and import cycles belong to the compiler and
`go vet`. Do not claim a file will not build.

## Step 3: what to check

These are the findings that past reviews got right and that authors fixed.

1. **Docs and code agree.** A godoc comment, `docs/` page, `CHANGELOG.md` entry
   or glossary row must not promise more than the code does. Every config value
   the docs show must be accepted by the parser. A user-visible change has an
   entry under `## [Unreleased]` in `CHANGELOG.md`.
2. **Lists that must agree, agree.** When one list of kinds, keys or fields
   changes, find its twins: both freeze guards (PLUGIN.md), the OpenAPI spec in
   `spec/httpapi/`, the manager projection, the conformance suites, the docs
   tables.
3. **Fail closed.** Invalid, foreign, typed-nil, negative, zero-where-unset or
   trailing input is rejected with a classified error. It is never silently
   defaulted or accepted.
4. **Normalised forms match the runtime.** A canonical or hashed form must treat
   a value exactly as the runtime reads it (number precision, `-0.0`, empty vs
   absent).
5. **Delivery guarantees hold.** At-least-once paths never drop, dead-letter or
   ack in-flight work to make shutdown or reload simpler. Settlement happens
   once and on a context that outlives the caller's cancel.
6. **Exclusivity and ownership survive changes.** A reload, swap or rollout
   that moves a session between exclusive and non-exclusive stops the old
   consumer before the new one starts. A lease or fencing token is checked
   before every write it guards.
7. **Waits and locks.** State is re-checked after a wait. A metric is read
   under the same lock as the state it describes. `time.Duration` math that
   can overflow compares by subtraction. Every network wait is bounded.
8. **Errors keep their class.** A new sentinel in `domain/shared` is pinned in
   `domain/shared/errors_test.go`. Callers compare with `errors.Is` or the
   `BridgeError` `Code` / `Class`, never by message text.
9. **Tests pin the change.** A fix has a test that fails without it. The test
   asserts the exact value, not a substring. See `tests.instructions.md`.
10. **No planning identifiers anywhere.** Lint gates Go source, tests
    included, for the known forms. Review covers what it cannot see: Markdown,
    YAML, shell, file names, and new forms such as `T14` or a `S10` suffix.
11. **Secrets stay secret.** Credentials are loaded through `credentials_uri`
    and credential stores, never logged, and never put in an error message or
    metric label.

## Do not suggest

Each of these was raised before, then declined with a reason.

- Adding `jsii.Close()` back to CDK tests (TESTS.md §2.7).
- Editing or merging existing `UBIQUITOUS.md` rows. The glossary is
  append-only; a change is a new `(correction)` row.
- Treating a deliberately cautious choice as a bug when the code or its comment
  says why it errs on the safe side.
- Merging two distinct concepts because they look alike — for example identity
  exclusivity and route ownership, or `SourceSessionID` and the route session.
- Dead-lettering in-flight at-least-once work to simplify a shutdown or reload.
- Widening a content-identity or reserved-header rule past the boundary its ADR
  draws.
- Deleting or rewriting plan files while the plan is still in progress.
- Renaming shared test fixtures for style alone.

## Before you post a finding

- Verify the premise. Check the arithmetic, the standard-library behaviour and
  the actual caller before claiming a bug. A wrong premise costs the author a
  round trip.
- Group the same problem in several places into one comment that lists them.
- Skip style-only comments. `gofmt` and lint own formatting.

Write each finding in the house format from `LANGUAGE.md`:

```
<file>:<line> <severity> <category>: <problem>. <fix>.
```

Severity is one of `BLOCKER`, `HIGH`, `MEDIUM`, `LOW`, `NIT`. Category is one
of `correctness`, `security`, `architecture`, `resilience`, `observability`,
`test-gap`, `maintainability`, `clarity`.

## Where the reasoning lives

| Question | Read |
|---|---|
| Why is it built this way? | `docs/adr/` (check `docs/adr/README.md` for superseded ADRs) |
| What does this term mean? | `UBIQUITOUS.md` |
| How must a plugin or its config look? | `PLUGIN.md` |
| What makes a test acceptable? | `TESTS.md` (§9 is the review checklist) |
| What does each checker cover? | `LINT.md` |
| How does a transport behave? | `docs/transports/` |
| How does a message flow through the bridge? | `docs/internals/` |
