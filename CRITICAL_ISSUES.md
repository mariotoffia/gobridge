# Critical Issues

Open findings from publishing the CDK modules (2026-08-27). Everything here is
**verified**, not suspected — each entry records the evidence and, where the fix
is known, the exact change. Delete an entry when it is fixed; delete the file
when it is empty.

Nothing in the codebase references this file. It is a worklist, not a durable
reference — see the "never reference a planning document" rule in
`.claude/CLAUDE.md`.

---

## 1. The container image is blocked by HIGH CVEs in the Go standard library

**Severity: high — no image has been published since v0.3.3.**

`Dockerfile:30` pins the builder to `golang:1.25-bookworm@sha256:ea341baa…`,
which ships **Go 1.25.12**. Trivy fails the image job on HIGH/CRITICAL findings,
including unfixed ones, and the binary carries six:

| CVE | Package |
|---|---|
| CVE-2026-33818 | `encoding/asn1` — DoS |
| CVE-2026-39821 | — |
| CVE-2026-56853 | `net/http` — unencrypted HTTP/2 DoS |
| CVE-2026-56858 | `html/template` — XSS via pathological input |
| CVE-2026-56859 | `encoding/xml` — DoS via decoding recursion |
| CVE-2026-56860 | `net/url` — DoS via quadratic path complexity |
| CVE-2026-56862 | `crypto/tls` — DoS via indefinite KeyUpdate |

Trivy reports the fixed versions as `1.25.13, 1.26.6, 1.27.0-rc.3`. Only
`stdlib` is flagged — the distroless runtime base has no findings, so bumping
the builder resolves all of them.

**Fix, already verified:**

```dockerfile
FROM golang:1.25-bookworm@sha256:3b4a11519ad929d1e1d261a12cff056f0c85b735253d7d861346b9c6f8b36437 AS build
```

Checked against the review workflow in the Dockerfile's own comment block:

- `docker run … go version` → `go1.25.14` (above the 1.25.13 floor)
- index covers `linux/amd64` **and** `linux/arm64`, which the image job requires

**Why it needs a new version train.** A tag-triggered workflow builds from the
tagged commit, so `cmd/gobridge/v0.3.6` will rebuild the old Dockerfile however
many times it is re-run. The fix reaches an image only via a new tag.

**Do not hand-push an image around this.** The scanner is not being pedantic —
it found real `net/http` and `crypto/tls` denial-of-service bugs. Publishing by
hand would bypass the digest-only push, SBOM, provenance and dual-platform scan
that `RELEASE.md` → "Image publication" requires.

---

## 2. Three published trains, none with a container image

| Version | Module tags | Strict gate | Consumer smoke | Image / Release / `latest` |
|---|---|---|---|---|
| v0.3.4 | 33/33 | pass | **fail** (smoke defect, since fixed) | none |
| v0.3.5 | **31/33** | layer 2 failed | not reached | none |
| v0.3.6 | 33/33 | pass | pass | **blocked by issue 1** |

All three sets of module tags are immutable and correct. v0.3.4 and v0.3.6 are
fully consumable — verified by building the documented CDK quickstart as a
third-party module against the public proxy with cold caches and no `replace`
directives.

### 2a. v0.3.5 is a partial train and will stay that way

31 of 33 tags exist. `deployment/aws-filebased-config/cdk/v0.3.5` and
`cmd/gobridge/v0.3.5` were **never created** — the train died at layer 2.

This breaks the "one version for everything" promise for that version alone:
`go get …/adapters/aws/store@v0.3.5` succeeds while `go get …/cdk@v0.3.5`
fails. Tags cannot be deleted under the repository's tag ruleset, and completing
the train now would tag two modules from a much later commit than its other 31.

**Decide and record one of:**

- complete it, accepting the commit-lineage skew, or
- document v0.3.5 as withdrawn in `CHANGELOG.md` and point readers at v0.3.6.

Leaving it undocumented is the worst option: the tags are discoverable and
nothing explains them.

---

## 3. Planning identifiers throughout comments and test names

**Severity: medium — it violates the repository's own hard rule.**

`.claude/CLAUDE.md` → "Never reference a planning document" forbids review or
task identifiers in comments, test names, file names and docs, because the plan
gets deleted and the reference outlives it. Current state:

- **34** test functions named after a batch rather than a behaviour, e.g.
  `Test_T20_Efs_FileSystem_BaselineProps`
- **182** comment references of the form `(Finding 5)`, `Phase-6`, `Phase-5A`
  across **69** Go files, concentrated in
  `adapters/aws/transport/sqs/acl_delivery.go` and
  `deployment/aws-filebased-config/lib/`

None of these point anywhere any more. Each should become either a plain-English
statement of the rule and its reason, or a reference to an ADR / a `docs/` page /
a `UBIQUITOUS.md` term.

This is mechanical but large, and renaming exported test functions is safe, so
it suits a dedicated pass rather than being folded into feature work.

---

## 4. The jsii process race is mitigated here, not fixed upstream

`jsii.Close()` shuts the child's stdin; the Node runtime exits; that wakes the
background `cmd.Wait()` jsii keeps on the child, which reaps descriptors
`Close()` has already freed — outside jsii's own mutex. Go 1.26's `os/exec`
dereferences the freed pipe state and the process dies with SIGSEGV.

Mitigated by closing the kernel once per test binary (`jsii_kernel_test.go` in
each CDK test package) instead of 154 times. Stress: 8 full-suite runs under
8-way CPU contention, zero crashes; previously ~1 in 12.

**Still open:**

- The upstream race is unchanged as of `jsii-runtime-go v1.140.0` — the code at
  `internal/kernel/process/process.go:181` is byte-identical to v1.127.0. A
  version bump does not help. Worth reporting upstream.
- `deployment/aws-filebased-config/cdk/integration/harness_fixture_jsii_test.go`
  still closes per test. It is behind `integration_aws`, needs real AWS, and was
  left alone because it could not be exercised here.

---

## 5. Proxy propagation is uneven and the train has to budget for it

`adapters/aws/store` and `adapters/native/store` — the only published modules
whose directories contain nested modules — consistently take **15–20 minutes**
to appear on `proxy.golang.org`, where a leaf module takes about **one**. At the
old 10-minute budget they failed their own release workflow while being entirely
correct, costing v0.3.4, v0.3.5 and v0.3.6 their layer 2.

Handled by raising the budget to 20 minutes and giving `wait_for_layer_workflows`
the same bounded one-retry `publish_module` already had.

**Still unexplained:** *why* those two are slow. The obvious theory — that
resolving nested children makes the proxy cache a miss for the parent prefix —
was tested and **disproved**: requesting `@v/list` right after pushing did not
change anything, because that endpoint is itself served from the proxy's cache
(`v0.3.6` was absent from it minutes after the tag existed). The current fix
absorbs the delay rather than removing it. If a train ever exceeds 20 minutes,
this needs a real answer rather than another budget increase.

---

## 6. `make test` coverage drifted silently

The non-race CDK pass listed packages by hand, and the list had fallen behind:
`./constructs`, `./constructs/gobridgecluster`, `./constructs/gobridgesingle`
and `./gobridgecdk` ran in **no target at all**. That is how a SIGSEGV in
`./constructs` first surfaced inside an irreversible release gate rather than in
CI.

Fixed by running the whole module (`./...`), which cannot drift.

**Worth a wider check:** this was found by accident. Other hand-maintained
package lists in the `Makefile` may have drifted the same way, and nothing
currently detects it.
