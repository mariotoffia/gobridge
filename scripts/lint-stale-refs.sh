#!/usr/bin/env bash
#
# lint-stale-refs.sh — find references to specifications that no longer exist.
#
# GoBridge was built through a long series of throw-away planning documents
# (FIX-003.md, ARCH_REVIEW.md, PROD_READY_ISSUES.md, chunk/round worklists …).
# Those documents are gone. The references to them are not: they survive as
# comments, test names, file names and doc prose that point a reader at a
# specification they can never open.
#
# The only durable references this repo allows are:
#
#   * the ADRs under docs/adr/
#   * the root-level canonical docs (ARCHITECTURE.md, DDD.md, UBIQUITOUS.md,
#     PLUGIN.md, TESTS.md, LINT.md, DEVELOPMENT.md, MODULES.md, RELEASE.md,
#     LANGUAGE.md, AGENTS.md, README.md, CHANGELOG.md)
#   * a doc under docs/ that actually exists on disk
#
# Everything else — "HIGH-3", "FIX 3 (XCUT-A)", "MEDIUM-9 / MEDIUM-10",
# "chunk-07", "see the plan" — is a dead pointer and must be rewritten as plain
# English that explains the RULE instead of naming the TICKET.
#
# This is a finder, not a gate. It reports candidates; a human reads each one
# and decides. It never fails the build.
#
# Usage:
#   scripts/lint-stale-refs.sh            # scan, write reports/stale-refs.log
#   scripts/lint-stale-refs.sh --summary  # counts only, no per-hit detail
#
# Exit code: always 0 (advisory), 2 on usage/internal error.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
readonly REPORT="$ROOT/reports/stale-refs.log"

command -v rg >/dev/null 2>&1 || { echo "lint-stale-refs: ripgrep (rg) is required" >&2; exit 2; }

summary_only=0
case "${1:-}" in
	--summary) summary_only=1 ;;
	"") ;;
	*) echo "usage: $0 [--summary]" >&2; exit 2 ;;
esac

cd "$ROOT"
mkdir -p reports

# Directories that are not GoBridge's own prose: vendored agent definitions, the
# pre-rewrite legacy tree, scratch design notes, and generated reports.
readonly EXCLUDES=(
	-g '!.git/**' -g '!legacy/**' -g '!.claude/**' -g '!.cursor/**'
	-g '!.superpowers/**' -g '!_design/**' -g '!reports/**'
	-g '!node_modules/**' -g '!vendor/**' -g '!*.log'
	-g '!scripts/lint-stale-refs.sh' # the checker's own patterns are not hits
)

# The live remediation worklist. These files DEFINE the identifiers, so every
# hit inside them is a definition rather than a stale reference. Delete these
# docs when the work lands and this exclusion becomes inert.
readonly SCRATCH=(
	-g '!PROD_READY_ISSUES.md' -g '!PROD_READY_PLAN.md' -g '!PROMPT-TEMPLATE.md'
	-g '!DOC_ISSUES.md'
)

# ── Detector patterns ────────────────────────────────────────────────────────

# Severity/task identifiers: HIGH-3, MEDIUM 9, CRITICAL 1, LOW-6, FIX-003,
# FIX 3, XCUT-A, ARCH-003, TASK-12.
readonly RE_TASKID='\b(CRITICAL|HIGH|MEDIUM|LOW|FIX|XCUT|TASK|ISSUE|ARCH)[-–_ ]?[0-9]{1,3}\b|\bXCUT-[A-Z]\b|\bT[0-9]{2}(–|-)T[0-9]{2}\b'

# Single-letter finding ids from deleted review documents: "finding F2", "(G2)",
# "the W-9 finding", "I2/I7 regression". AMBIGUOUS BY DESIGN — the same shapes
# are legitimate when a live spec defines them in its own tables, so the two
# spec trees that do that are excluded and diagram lines are filtered out.
readonly RE_FINDING='\b(finding|findings|gap|item)s? [A-Z]{1,4}-?[0-9]{1,3}\b|\b[FWGIR]-?[0-9]{1,2}\b|\bN[0-9]{1,2}\b'
readonly SPEC_LOCAL=(-g '!docs/cluster/spec/**' -g '!docs/scenarios/**' -g '!docs/adr/**')
# Mermaid/ASCII diagram lines mint node ids that look exactly like finding ids.
readonly RE_DIAGRAM='(-->|->>|-\.->|participant |subgraph |\|--|^\s*[A-Z][0-9]\[)'

# Batch/iteration labels from the old worklists: chunk-07, chunk12, round3,
# phase 5 of, batch 2 of.
readonly RE_BATCH='\b(chunk|round|batch|wave)[-–_ ]?[0-9]{1,2}\b|\bphase [0-9] of\b|\bprod[-_]?ready[-_ ]?(chunk|c|r)[0-9]+\b'

# Prose that points at an unnamed document.
readonly RE_VAGUE='\b(see|per|from) the (original )?(plan|spec|specification|design doc(ument)?|remediation plan|task ?list)\b|\b(in|to|of) the (design doc(ument)?|original plan|remediation plan|specification)\b|\bas (specified|designed|planned|agreed) in the\b|\bthe (above|attached|linked) (plan|spec)\b'

# File names that carry a dead task id in the name itself.
readonly RE_FILENAME='(chunk[0-9_-]|prod_?ready|prod_fixes|followup_fixes|round[0-9]|_high[0-9]|_c[0-9]+_|residuals)'

# ── Helpers ──────────────────────────────────────────────────────────────────

section() { printf '\n%s\n%s\n' "$1" "$(printf '=%.0s' $(seq ${#1}))"; }

# hits <regex> — one "path:line:text" per match, scratch docs excluded.
# The trailing "." matters: without an explicit path rg reads stdin when stdin
# is not a tty, and the script hangs forever under CI or a non-interactive shell.
hits() { rg -n --no-heading --color never "${EXCLUDES[@]}" "${SCRATCH[@]}" -e "$1" . || true; }

# ── Detector 4 needs the set of documents this repo actually has ─────────────

DOCLIST="$(mktemp)"
trap 'rm -f "$DOCLIST"' EXIT
find . \( -type f -o -type l \) \( -name '*.md' -o -name '*.adoc' \) \
	-not -path './.git/*' -not -path './node_modules/*' -not -path './vendor/*' \
	-exec basename {} \; | sort -u >"$DOCLIST"
existing_docs="$(cat "$DOCLIST")"

# Documents git knows were deleted — the highest-confidence dead targets.
deleted_docs="$(git log --diff-filter=D --name-only --pretty=format: -- '*.md' 2>/dev/null \
	| sed 's#.*/##' | sort -u | grep -v '^$' || true)"
# A name that was deleted and later re-added is not dead.
deleted_docs="$(comm -23 <(printf '%s\n' "$deleted_docs") <(printf '%s\n' "$existing_docs"))"

# ── Report ───────────────────────────────────────────────────────────────────

{
	echo "stale-refs — references to specifications that no longer exist"
	echo "generated by scripts/lint-stale-refs.sh"
	echo
	echo "Scanned: the whole repo except .git, legacy/, .claude/, .cursor/,"
	echo "         .superpowers/, _design/, reports/, vendor/, node_modules/."
	echo "Skipped: PROD_READY_ISSUES.md, PROD_READY_PLAN.md, PROMPT-TEMPLATE.md,"
	echo "         DOC_ISSUES.md — the live worklist defines these ids."
	echo
	echo "Each hit is a CANDIDATE. Read it, then either rewrite it in plain"
	echo "English or leave it if it is genuinely not a spec reference."

	section "1. Task / severity identifiers  (HIGH-3, FIX 3 (XCUT-A), LOW-6 …)"
	echo "Rewrite as the rule itself: what must hold, and why."
	echo
	taskid="$(hits "$RE_TASKID")"
	[[ $summary_only -eq 1 ]] || printf '%s\n' "$taskid"

	section "2. Single-letter finding ids  (finding F2, (G2), W-9, I2/I7 …)"
	echo "These came from review documents that were deleted. Excluded here:"
	echo "docs/cluster/spec/, docs/scenarios/, docs/adr/ — those define their own"
	echo "F/G/I tables in-document, which is allowed. Diagram lines are filtered."
	echo
	finding="$(rg -n --no-heading --color never "${EXCLUDES[@]}" "${SCRATCH[@]}" "${SPEC_LOCAL[@]}" \
		-e "$RE_FINDING" . | rg -v "$RE_DIAGRAM" || true)"
	[[ $summary_only -eq 1 ]] || printf '%s\n' "$finding"

	section "3. Batch / iteration labels  (chunk-07, round3, phase 5 of …)"
	echo "These name a work batch that no longer exists. Drop the label."
	echo
	batch="$(hits "$RE_BATCH")"
	[[ $summary_only -eq 1 ]] || printf '%s\n' "$batch"

	section "4. References to documents that are not on disk"
	echo "A <name>.md or <name>.adoc mentioned in text with no such file in the repo."
	echo
	deadfile="$(hits '\b[A-Za-z0-9_][A-Za-z0-9_./-]*\.(md|adoc)\b' | awk -v docs="$DOCLIST" '
		BEGIN { while ((getline d < docs) > 0) if (d != "") have[d] = 1 }
		{
			text = $0
			while (match(text, /[A-Za-z0-9_][A-Za-z0-9_.\/-]*\.(md|adoc)/)) {
				ref  = substr(text, RSTART, RLENGTH)
				text = substr(text, RSTART + RLENGTH)
				sub(/.*\//, "", ref)
				if (!(ref in have)) { print; next }
			}
		}')"
	[[ $summary_only -eq 1 ]] || printf '%s\n' "$deadfile"

	section "5. Bare mentions of deleted documents  (no extension)"
	echo "Document names git recorded as deleted, mentioned without '.md'."
	echo
	deadname=""
	# One alternation over every deleted stem — dots escaped, generic names dropped.
	stem_re="$(printf '%s\n' "$deleted_docs" \
		| sed -e 's/\.md$//' -e 's/\./\\./g' \
		| awk 'length($0) >= 6 && $0 !~ /^(README|index|overview|MISSING|TASKS|CHANGELOG)$/' \
		| paste -sd'|' -)"
	[[ -n $stem_re ]] && deadname="$(hits "\b(${stem_re})\b")"
	[[ $summary_only -eq 1 ]] || printf '%s\n' "$deadname"

	section "6. Prose pointing at an unnamed document"
	echo "\"see the plan\", \"per the spec\" — name the rule instead."
	echo
	vague="$(hits "$RE_VAGUE")"
	[[ $summary_only -eq 1 ]] || printf '%s\n' "$vague"

	section "7. File names that carry a dead task id"
	echo "The file name itself advertises a batch nobody can look up. Rename."
	echo
	filename="$(rg --files "${EXCLUDES[@]}" "${SCRATCH[@]}" . | rg -i "$RE_FILENAME" || true)"
	printf '%s\n' "$filename"

	# ── Summary ──────────────────────────────────────────────────────────
	# awk, not bash string ops: ${v//[[:space:]]/} on a 100 KB result is glacial.
	count() { awk 'NF' <<<"${1:-}" | wc -l | tr -d ' '; }
	files() { awk 'NF' <<<"${1:-}" | cut -d: -f1 | sort -u | wc -l | tr -d ' '; }

	section "Summary"
	printf '%-46s %6s %6s\n' "detector" "hits" "files"
	printf '%-46s %6s %6s\n' "----------------------------------------------" "------" "------"
	printf '%-46s %6s %6s\n' "1. task / severity identifiers"    "$(count "$taskid")"   "$(files "$taskid")"
	printf '%-46s %6s %6s\n' "2. single-letter finding ids"      "$(count "$finding")"  "$(files "$finding")"
	printf '%-46s %6s %6s\n' "3. batch / iteration labels"       "$(count "$batch")"    "$(files "$batch")"
	printf '%-46s %6s %6s\n' "4. refs to missing documents"      "$(count "$deadfile")" "$(files "$deadfile")"
	printf '%-46s %6s %6s\n' "5. bare mentions of deleted docs"  "$(count "$deadname")" "$(files "$deadname")"
	printf '%-46s %6s %6s\n' "6. prose pointing at no document"  "$(count "$vague")"    "$(files "$vague")"
	printf '%-46s %6s %6s\n' "7. file names with a dead task id" "$(count "$filename")" "$(count "$filename")"

	section "Worst offenders (detectors 1-3, by hit count)"
	printf '%s\n%s\n%s\n' "$taskid" "$finding" "$batch" | grep -v '^$' | cut -d: -f1 | sort | uniq -c | sort -rn | head -30
} >"$REPORT" 2>&1

tail -n 24 "$REPORT"
echo
echo "full report: reports/stale-refs.log ($(wc -l <"$REPORT" | tr -d ' ') lines)"
