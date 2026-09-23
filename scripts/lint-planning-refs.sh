#!/usr/bin/env bash
#
# lint-planning-refs.sh — reject references to planning documents in Go source.
#
# GoBridge is built through throw-away worklists: chunk plans, reconfiguration
# rounds, numbered review findings, per-chunk rule slugs. Those documents are
# deleted when the work lands. The references to them are not — they survive as
# comments pointing a reader at a specification they can never open:
#
#     // Release them here (Finding 2).
#     // ... rather than refusing (design Phase-4 residual)
#     // c13-claim-quadratic: bound the scan
#
# A reader meeting one of those learns nothing except that something was decided
# somewhere else. AGENTS.md therefore allows only DURABLE references: an ADR, a
# canonical root document plus section, a live page under docs/, a UBIQUITOUS.md
# term — or, best, the rule itself written out in plain English.
#
# This check greps every .go file — test files included — for the token shapes
# a planning document mints. Unlike its advisory predecessor it is a GATE: there
# is no annotation that sanctions a hit, because there is no case where a
# comment, a test name or an assertion message must name a deleted worklist.
#
# Scope: every .go file, _test.go included, outside .git, .worktrees and vendor.
#
# Usage:
#   scripts/lint-planning-refs.sh              # scan; exit 0 clean, 1 on violations
#   scripts/lint-planning-refs.sh --self-test  # prove the checker has teeth, then scan
#
# Exit codes: 0 = clean / self-test passed, 1 = violations found, 2 = usage error.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Scratch tree used only by --self-test; removed on exit (guarded for set -u).
SELFTEST_TMP=""
trap 'if [ -n "${SELFTEST_TMP:-}" ]; then rm -rf "$SELFTEST_TMP"; fi' EXIT

# ── Detector patterns ────────────────────────────────────────────────────────
#
# Batch and iteration labels: "Chunk 13", "chunk-7", "RECONFIG-1", "Phase-4",
# "round 2", and the per-chunk rule slugs "c13-claim-quadratic". "batch N" is
# deliberately NOT here: a batch is a real thing this bridge sends, and
# "batch-0" is ordinary fixture data, not a worklist label.
readonly RE_BATCH='\b[Cc]hunk[ _-]?[0-9]+|\bRECONFIG-[0-9]+|\bPhase-[0-9]+|\b(round|wave)[ _-][0-9]+\b|\bc[0-9]{1,2}-[a-z][a-z0-9]*(-[a-z0-9]+)+'

# Review findings, numbered ("Finding 8", "cluster finding 11", "finding F2")
# or slugged ("finding: desired-ahead-of-applied"). Both name an entry in a
# review document that no longer exists.
readonly RE_FINDING='\b[Ff]indings?[ _-][0-9]+|\b[Ff]inding [A-Z]-?[0-9]+|\b[Ff]indings?:'

# Severity/task identifiers from deleted review documents.
readonly RE_SEVERITY='\b(CRITICAL|HIGH|MEDIUM|MED|LOW|FIX|XCUT|TASK|ISSUE|ARCH)[-_ ][0-9]{1,3}\b'

# Prefixed ticket IDs from deleted review and QA worklists — "BUG-3", "RES-007",
# "SEC-1", "GAP-10", "REV-2-topowarn", "TEST-3", "API-1", the compound
# "MQTT-OBS-2", "CORE-RES-1" and "H-OBS DLQ-1" — and the bare reviewer tags
# "(ADV)" and "(QA)". The prefixes are listed, never generalised to
# [A-Z]+-[0-9]+: UTF-8, SHA-256, ADR-0013, UC-CR7 and fixture names such as
# SQS-OUT-1 or ORDER-KEY-1 share that shape and are legitimate.
readonly RE_TICKET='\b(BUG|RES|SEC|GAP|REV|TEST|API|OBS)-[0-9]+|\bH-OBS\b|\((ADV|QA)\)'

# Prose pointing at a document that is not in the repository.
readonly RE_DOCPTR='\bdesign (doc|document|§|Phase)|\b(see|per|from|in) the (plan|spec|specification|design doc)\b|\bValidation Matrix\b'

# scan_tree <root> prints one "<file>:<line>: <source line>" per violation.
# It is silent and returns 0 when the tree is clean, 1 when it is not; the
# caller counts the lines, because an exit status cannot carry a count past 255.
scan_tree() {
	local root="$1" matches
	matches="$(grep -rnE --include='*.go' \
		-e "$RE_BATCH" -e "$RE_FINDING" -e "$RE_SEVERITY" -e "$RE_TICKET" -e "$RE_DOCPTR" \
		"$root" 2>/dev/null |
		grep -v '/\.git/' |
		grep -v '/\.worktrees/' |
		grep -v '/vendor/' || true)"

	[ -z "$matches" ] && return 0

	# Print repo-relative paths so the output is a clickable file:line.
	printf '%s\n' "$matches" | sed -e "s#^$root/##" -e 's/:[[:space:]]*/: /2'
	return 1
}

run_repo_scan() {
	local output violations
	if output="$(scan_tree "$ROOT")"; then
		echo "planning references: OK (no planning-document identifiers in Go source or tests)"
		return 0
	fi
	violations="$(printf '%s\n' "$output" | wc -l | tr -d ' ')"
	echo "planning references: FAIL ($violations planning-document identifier(s) in Go source or tests)" >&2
	echo "" >&2
	printf '%s\n' "$output" >&2
	echo "" >&2
	echo "Fix: write the RULE in plain English — what must hold, and why — or cite a" >&2
	echo "durable reference (an ADR, a canonical root doc plus section, a live page" >&2
	echo "under docs/, a UBIQUITOUS.md term). See AGENTS.md." >&2
	return 1
}

self_test() {
	# A global with an EXIT trap, not a local with a RETURN trap: bash evaluates
	# a RETURN trap after the local scope is gone, which trips `set -u`.
	SELFTEST_TMP="$(mktemp -d)"
	local tmp="$SELFTEST_TMP"

	mkdir -p "$tmp/pkg"

	# Negative cases: each token shape MUST be flagged.
	local case
	for case in \
		'// Release the handles here (Finding 2).' \
		'// Rejected at load (Chunk 13).' \
		'// The applier owns this (RECONFIG-1).' \
		'// Seeded at deploy time (design Phase-4 residual).' \
		'// c13-claim-quadratic: bound the partition scan.' \
		'// Ordering follows the Validation Matrix.' \
		'// Behaviour is defined in the design doc.' \
		'// Covers the HIGH-3 rule.' \
		'// Keyed on the version (finding: stale acks regress running).' \
		'// Severity carried over from the review (MED-2).' \
		'// BUG-3: the drain must meter QoS 0 deliveries.' \
		'// Bounded retry budget (RES-007).' \
		'// Covers RES-003/004.' \
		'// Rejects a self-referencing key (SEC-001).' \
		'// SEC-1: validate the override reference.' \
		'// GAP-10: SQS delivery auto-extend boundary.' \
		'// REV-2-topowarn: warn on a lopsided topology.' \
		'// REV-3-routeiso: routes stay isolated.' \
		'// TEST-3 (shipped composition root).' \
		'// Also validates API-1.' \
		'// Capped below Full (MQTT-OBS-2).' \
		'// Latch the partition stalled (CORE-RES-1).' \
		'// Alarmable without a storage scan (H-OBS DLQ-1).' \
		'// Still works when the exchange already exists (ADV).' \
		'// Tests for the Locate method (QA).' \
		't.Errorf("BUG-3: got %d, want 1", n)'; do
		printf 'package pkg\n\n%s\nconst X = 1\n' "$case" >"$tmp/pkg/a.go"
		if scan_tree "$tmp" >/dev/null; then
			echo "self-test FAIL: checker did not flag: $case" >&2
			return 1
		fi
	done

	# Positive cases: durable references and ordinary prose MUST pass.
	for case in \
		'// See ADR-0013 — a coordinated rollout commits behind a barrier.' \
		'// ARCHITECTURE.md §2 places this in Layer 3.' \
		'// docs/cluster/spec/cluster-config-rollout-protocol.md §7 defines UC-CR7.' \
		'// A stale claim is reclaimed by a higher fencing version, always immediately.' \
		'// Phase 1 of the two-phase commit prepares; phase 2 applies.' \
		'// Reads the body in 4096-byte pieces.' \
		'// Payloads are UTF-8; digests are SHA-256.' \
		'// UC-CR7 is defined in docs/cluster/spec/cluster-config-rollout-protocol.md.' \
		'const q = "SQS-OUT-1" // fixture queue name' \
		'const stage = "SQS-STAGE-0" // diagram node' \
		'const k = "ORDER-KEY-1" // fixture ordering key' \
		'const secret = "SECRET-KEY-12345" // fixture secret' \
		'const fixture = "batch-0" // a batch is a real thing, not a worklist label'; do
		printf 'package pkg\n\n%s\nconst X = 1\n' "$case" >"$tmp/pkg/a.go"
		if ! scan_tree "$tmp" >/dev/null; then
			echo "self-test FAIL: checker flagged a durable reference: $case" >&2
			scan_tree "$tmp" >&2
			return 1
		fi
	done

	# A _test.go file is in scope and MUST be flagged.
	rm -f "$tmp/pkg/a.go"
	printf 'package pkg\n\n// Covers Finding 2.\nconst X = 1\n' >"$tmp/pkg/a_test.go"
	if scan_tree "$tmp" >/dev/null; then
		echo "self-test FAIL: checker did not flag a _test.go file" >&2
		return 1
	fi
	rm -f "$tmp/pkg/a_test.go"

	echo "planning-reference self-test: PASS (flags every planning shape in source and tests, allows durable references)"
	run_repo_scan
}

main() {
	case "${1:-}" in
	--self-test) self_test ;;
	"") run_repo_scan ;;
	-h | --help) sed -n '2,30p' "${BASH_SOURCE[0]}" ;;
	*)
		echo "unknown argument: $1" >&2
		echo "usage: $0 [--self-test]" >&2
		return 2
		;;
	esac
}

main "$@"
