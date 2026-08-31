# GoBridge — Agent Communication Language

This file is **must-read** for every AI coding agent operating in this repo. It defines the default communication style — professional, terse, technically precise. It applies to **agent-to-human** and **agent-to-agent** communication: chat replies, status updates, review findings, handoffs.

It does NOT apply to user-facing artifacts produced by the agent on the user's behalf — code, documentation, commit messages, PR descriptions — unless the user explicitly requests terse output for that artifact. See [Boundaries](#boundaries).

**MUST:** Always use plain, simple english when documenting or communicating with the user, do assume the user is not well versed in the inner workings of gobridge. 

## Persistence

Always-on. Every response in this repo defaults to terse mode. There is no "current response only" — the rule is repository-scoped, not invocation-scoped.

If the user asks for `normal mode`, `more detail`, `verbose`, or equivalent, switch off terse for that response and return to terse on the following response unless told otherwise.

## Discipline

- Keep full technical substance. Remove filler.
- Do not switch into exaggerated style.
- Do not use broken grammar.
- Do not remove important precision.
- Do not compress code, commands, API names, error strings, file paths, function names, security warnings, or ordered steps.

## Rules

Remove:

- filler: just, really, basically, actually, simply
- pleasantries: sure, certainly, of course, happy to
- unnecessary hedging
- repeated summaries
- motivational language
- broad commentary without action

Keep:

- articles and normal grammar
- compact full sentences
- exact technical terms
- exact file names, paths, symbols, APIs, commands, and error strings
- uncertainty when evidence is incomplete
- enough context to avoid ambiguity

Prefer:

- short paragraphs
- direct statements
- actionable bullets
- concrete next steps
- file/line references when available
- explicit risk and severity labels when reviewing

Use fragments only in structured fields, status reports, and review findings.

## Pattern

Prefer:

```text
<thing> <finding>. <reason>. <required action>.
```

Bad:

```text
Sure! I'd be happy to help you with that. The issue you're experiencing is likely caused by the auth middleware.
```

Good:

```text
Auth middleware bug. Token expiry check uses `<` instead of `<=`. Fix comparison and add boundary test.
```

## Review Output Format

When reviewing code, specs, designs, tasks, or architecture, prefer this format:

```text
<file-or-section>:<line-or-heading> <severity> <category>: <problem>. <required fix>.
```

Severity:

```text
BLOCKER | HIGH | MEDIUM | LOW | NIT
```

Categories:

```text
correctness | security | architecture | resilience | observability | test-gap | maintainability | clarity
```

Example:

```text
ARCHITECTURE.md:§4 HIGH architecture: Domain depends on adapters/aws. Invert dep via ports.Sender.
```

If no findings:

```text
No findings.
```

## Agent Handoff Format

When handing work to another agent (or sub-subagent), use:

```text
Goal:
Constraints:
Files:
Checks:
Risks:
Done when:
```

Keep each section short.

## Status Format

When reporting progress, use:

```text
State:
Changed:
Checks:
Blocked:
Next:
```

Omit sections that do not apply.

## Auto-Clarity (BLOCKING — overrides terse)

Use normal full clarity when terse compression could cause mistakes. These contexts are non-negotiable carve-outs:

- security warnings
- destructive operations (`rm -rf`, `git push --force`, `DROP TABLE`, mass file deletion)
- irreversible actions
- production deployment steps
- database / schema migrations
- IAM or permission changes
- multi-step sequences where order matters
- ambiguous requirements (clarify; do not guess in terse form)
- legal, compliance, privacy, or safety statements
- explanations the user has explicitly asked for in detail

After a clarity exception, return to compact professional style.

Example:

```text
Warning: This permanently deletes all rows in `users` and cannot be undone. Verify a recent backup before running it.
```

## Boundaries (BLOCKING — terse does NOT apply to these artifacts)

Do NOT make any of the following unnaturally terse, even when terse mode is active:

- Production code (Go source, scripts, configuration files).
- Public documentation in this repo: `AGENTS.md`, `README.md`, `ARCHITECTURE.md`, `DDD.md`, `UBIQUITOUS.md`, `PLUGIN.md`, `TESTS.md`, `DEVELOPMENT.md`, `LANGUAGE.md` itself, anything under `docs/`, `_design/`, runbooks, asciidoc specs.
- Commit messages and PR descriptions.
- Code comments where the comment exists to explain non-obvious logic.
- AsciiDoc design specs under `_design/`.

These artifacts follow the existing repository style guides and reach a wider audience than the immediate session. Terse here would degrade the codebase.

The terse rule applies to the agent's spoken/chat output and intra-agent handoffs only. If the user explicitly asks for a terse commit message, terse PR description, or terse documentation section, honor that request for that specific artifact and return to normal style for unrelated artifacts in the same session.

## Off-switches

The user can disable terse for a response with any of:

- `normal mode`
- `verbose`
- `more detail`
- `explain in full`
- `stop terse mode`

After such an instruction, the next response is normal-mode. The response after that returns to terse unless the user says otherwise.
