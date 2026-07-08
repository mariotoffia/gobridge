#!/usr/bin/env bash
#
# lint-xbridge-headers.sh — governance for the x-bridge.* header namespace.
#
# Every x-bridge.* header the bridge emits is meant to be declared once, as a
# constant in domain/messaging (the single source of truth). Adapters
# occasionally must mint a transport-LOCAL x-bridge.* key — e.g. Azure Service
# Bus application properties that are stripped at ingress and never cross the
# domain boundary. Those are permitted, but ONLY when explicitly annotated so a
# reviewer can see the deviation at a glance.
#
# This check greps every x-bridge.* STRING LITERAL in non-test .go files OUTSIDE
# domain/messaging and asserts each one is either:
#
#   (a) the value of a header constant registered in domain/messaging, or
#   (b) annotated on the SAME LINE with the marker:
#
#         // x-bridge-local: <reason>
#
# Any other x-bridge.* literal is an ungoverned header and fails the check.
#
# Scope: non-_test.go files only — production minting is what governance cares
# about; tests routinely hardcode header strings. Pure grep/sed/awk, no build,
# no Go toolchain required.
#
# Usage:
#   scripts/lint-xbridge-headers.sh              # scan the repo; exit 0 clean, 1 on violations
#   scripts/lint-xbridge-headers.sh --self-test  # prove the checker has teeth, then scan; exit 0 on success
#
# Exit codes: 0 = clean / self-test passed, 1 = violations found, 2 = usage/internal error.

set -euo pipefail

readonly ANNOTATION='x-bridge-local:'
readonly LITERAL_RE='"x-bridge\.[^"]*"'

# repo root = parent of this script's directory.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Temp dir used only by --self-test; cleaned up on exit (guarded for set -u).
SELFTEST_TMP=""
trap 'if [ -n "${SELFTEST_TMP:-}" ]; then rm -rf "$SELFTEST_TMP"; fi' EXIT

# registered_values <headers.go> prints the sorted, unique set of x-bridge.*
# string values declared in domain/messaging (including the bare "x-bridge."
# prefix constant).
registered_values() {
	grep -oE "$LITERAL_RE" "$1" | tr -d '"' | sort -u
}

# scan_tree <root> <registered-values> prints one line per ungoverned literal in
# the form "<file>:<line>: <literal>", and returns the violation count as its
# exit status (0 = clean).
scan_tree() {
	local root="$1" registered="$2"
	local matches count=0

	# All x-bridge.* literals in non-test .go files, minus domain/messaging and
	# VCS/worktree copies. `|| true` keeps set -e happy when grep finds nothing.
	matches="$(grep -rn --include='*.go' "$LITERAL_RE" "$root" 2>/dev/null \
		| grep -v '/domain/messaging/' \
		| grep -v '_test\.go:' \
		| grep -v '/\.git/' \
		| grep -v '/\.worktrees/' || true)"

	[ -z "$matches" ] && return 0

	while IFS= read -r line; do
		[ -z "$line" ] && continue

		# An adapter-local annotation on the line sanctions every literal on it.
		if printf '%s' "$line" | grep -q "$ANNOTATION"; then
			continue
		fi

		local loc lit
		loc="$(printf '%s' "$line" | cut -d: -f1-2)"
		for lit in $(printf '%s' "$line" | grep -oE "$LITERAL_RE" | tr -d '"'); do
			if printf '%s\n' "$registered" | grep -qxF "$lit"; then
				continue
			fi
			printf '%s: %s\n' "$loc" "$lit"
			count=$((count + 1))
		done
	done <<-EOF
		$matches
	EOF

	return "$count"
}

run_repo_scan() {
	local registered violations output
	registered="$(registered_values "$ROOT/domain/messaging/headers.go")"

	if output="$(scan_tree "$ROOT" "$registered")"; then
		echo "x-bridge header governance: OK (no ungoverned x-bridge.* literals)"
		return 0
	else
		violations=$?
		echo "x-bridge header governance: FAIL ($violations ungoverned x-bridge.* literal(s))" >&2
		echo "" >&2
		printf '%s\n' "$output" >&2
		echo "" >&2
		echo "Fix: reference a domain/messaging constant, or annotate the line with '// $ANNOTATION <reason>'." >&2
		return 1
	fi
}

self_test() {
	local tmp registered
	registered="$(registered_values "$ROOT/domain/messaging/headers.go")"
	SELFTEST_TMP="$(mktemp -d)"
	tmp="$SELFTEST_TMP"

	# Negative case: an unannotated, unregistered key MUST be flagged.
	mkdir -p "$tmp/adapters/bogus"
	printf 'package bogus\n\nconst H = "x-bridge.self-test-bogus"\n' >"$tmp/adapters/bogus/h.go"
	if scan_tree "$tmp" "$registered" >/dev/null; then
		echo "self-test FAIL: checker did not flag an unannotated x-bridge.* literal" >&2
		return 1
	fi

	# Positive case A: the same key, annotated adapter-local, MUST pass.
	printf 'package bogus\n\nconst H = "x-bridge.self-test-bogus" // %s stripped at ingress\n' "$ANNOTATION" >"$tmp/adapters/bogus/h.go"
	if ! scan_tree "$tmp" "$registered" >/dev/null; then
		echo "self-test FAIL: checker flagged an annotated adapter-local literal" >&2
		return 1
	fi

	# Positive case B: a registered domain value re-typed as a literal MUST pass.
	printf 'package bogus\n\nconst H = "x-bridge.correlation-id"\n' >"$tmp/adapters/bogus/h.go"
	if ! scan_tree "$tmp" "$registered" >/dev/null; then
		echo "self-test FAIL: checker flagged a registered domain header value" >&2
		return 1
	fi

	echo "x-bridge header governance self-test: PASS (checker flags ungoverned keys, allows annotated + registered)"

	# Finally, the real repo must be clean.
	run_repo_scan
}

main() {
	case "${1:-}" in
	--self-test)
		self_test
		;;
	"")
		run_repo_scan
		;;
	-h | --help)
		sed -n '2,40p' "${BASH_SOURCE[0]}"
		;;
	*)
		echo "unknown argument: $1" >&2
		echo "usage: $0 [--self-test]" >&2
		return 2
		;;
	esac
}

main "$@"
