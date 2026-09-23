---
applyTo: "scripts/release/**,.github/workflows/**,Makefile"
---

# Release tooling and CI workflows

Sources: RELEASE.md, MODULES.md and the comments in `scripts/release/run.sh`.
These paths had no automated review before, and each rule below comes from a
release that broke without it.

- Release shell runs on a maintainer's Mac as often as in CI, and macOS ships
  bash 3.2. No associative arrays (`declare -A`), `mapfile` / `readarray`,
  `${var,,}` / `${var^^}`, or other bash-4-only features.
- Tags are pushed one at a time. A bulk tag push can silently skip triggering
  the release workflow for some tags, and a tag cannot be re-pushed.
- Polling GitHub stays one API call per cycle for the whole layer, not one
  poller per module, which exhausts the rate limit on large layers.
- The release workflow's privileged job (the one that writes the GitHub
  Release) stays free of checkout, Make and Docker, and nothing reintroduces
  container image publication. `scripts/release/workflow_security_test.go` pins
  both; a change here updates that test rather than weakening it.
- Every Make target that runs Go tests passes `-race` and `-count=1` and
  writes its report under `reports/` (TESTS.md §8).
- A published module has no `replace` directive (RELEASE.md).
