# Final DDD / Hexagonal / Clean Architecture Fix Plan

## Purpose

This plan closes the remaining gap between the current **8.5/10**
DDD/Hex/Clean grade and a realistic **9.5/10** target. It collects the
items from the deep analysis into actionable tasks: tighten
`go-arch-lint`, add a real `.golangci.yml`, build two small custom
analyzers, and wire advisory third-party tools into `make install`.

The plan deliberately **skips documentation symbol drift** (Tier 3.3 in
the analysis). Drift in `.md` files is real but is best handled by
review discipline; a Go-AST-vs-Markdown analyzer is too much
infrastructure for the value.

This is the third architecture plan in this repository:

| Plan | Status | Scope |
|---|---|---|
| `ARCHITECTURE_PLAN.md` | (informational) | Original system design |
| `ARCH_LINT_PLAN.md` | Done (24/24 tasks) | Lint policy and clean-break migration |
| `FINAL_DDD_HEX_CLEAN_FIX_PLAN.md` | THIS DOCUMENT | Final tightening for 9.5/10 |

---

## Non-negotiable design constraints (inherited)

The following constraints from `ARCH_LINT_PLAN.md` continue to apply
to every task in this plan:

- **MUST: No backward compatibility.** No deprecated aliases, no shim
  packages, no compatibility wrappers. Old API symbols are deleted in
  the same change that introduces the new one. Update every in-tree
  caller atomically.
- **Strict Clean / Hexagonal / DDD.** Every change must be expressible
  in those terms. No project-specific exceptions; if a rule conflicts
  with current code, the code is wrong, not the rule.
- **Every task leaves `make lint` and `make test` green.** Per task,
  not per plan.
- **Update the lint mapping regression test** (`scripts/lint-arch-mapping-test.sh`)
  whenever a new component is added.

---

## Task summary

| ID | Task | Tier | Estimated grade gain |
|---|---|---|---|
| FIX-001 | Lift `circuitbreaker` to a port; remove from adapter `mayDependOn` | 1 | +0.25 |
| FIX-002 | Ban `net/http` and `database/sql` from `ports/`; refactor `ports.HTTPMountable` | 1 | +0.25 |
| FIX-003 | Eliminate `bridge → config` and `httpapi → config` via a parsed-config DTO port | 1 | +0.25 |
| FIX-004 | Split `domain/` into bounded-context sub-components | 1 | +0.25 |
| FIX-005 | Add `.golangci.yml` with depguard, interfacebloat, ireturn, forbidigo, wrapcheck, gochecknoglobals/inits, revive | 2 | +0.5 |
| FIX-006 | Build the ACL-naming custom analyzer | 3.1 | (drift prevention) |
| FIX-007 | Build the aggregate-root marker analyzer | 3.2 | (design rigor) |
| FIX-008 | Wire `modgraphviz`, `dupl`, `goconst` into `make install`; add advisory targets | 4 | (review aid) |
| FIX-009 | Add the drift-verification workflow to `.claude/CLAUDE.md` | — | (process) |

`grade gain` is cumulative when applied in order. After FIX-001..005 the
project should sit at **9.5/10**.

---

# Tier 1 — `go-arch-lint` tightening

## FIX-001: Lift `circuitbreaker` to a port

### Problem

`adapters/mqtt/transport/paho/cb_sender.go` directly imports
`github.com/mariotoffia/gobridge/circuitbreaker`. The current
`.go-arch-lint.yml` has `adapter_transport_mqtt_paho.mayDependOn`
include `circuitbreaker` to permit this. That is a hexagonal leak: an
adapter reaches back into a project-internal utility instead of using a
port.

### Outcome

- New port `ports.CircuitBreaker` with the contract Paho actually
  needs (`Allow`, `OnSuccess`, `OnFailure(err error)`).
- The `circuitbreaker` package implements that port (and may move
  internals around without affecting adapters).
- Paho consumes `ports.CircuitBreaker` only.
- `.go-arch-lint.yml`: `adapter_transport_mqtt_paho.mayDependOn` no
  longer lists `circuitbreaker`.

### Steps

1. Read `adapters/mqtt/transport/paho/cb_sender.go` and inventory the
   exact `circuitbreaker.*` symbols used.
2. Add `ports/resilience.go`:

   ```go
   package ports

   // CircuitBreaker is a per-call gate that opens after enough failures
   // to protect a downstream from continued pressure. Adapters compose
   // a circuit breaker around outbound calls without knowing the
   // implementation.
   type CircuitBreaker interface {
       Allow() bool
       OnSuccess()
       OnFailure(err error)
   }
   ```
3. In `circuitbreaker/`, add a constructor that returns an instance
   satisfying `ports.CircuitBreaker`. Add a compile-time assertion:
   `var _ ports.CircuitBreaker = (*Breaker)(nil)`.
4. Refactor `cb_sender.go` to take a `ports.CircuitBreaker` constructor
   parameter instead of importing the `circuitbreaker` package.
5. Wire the concrete implementation in the composition root (`cmd/`
   and `deployment/`).
6. Edit `.go-arch-lint.yml`:
   ```yaml
   adapter_transport_mqtt_paho:
     mayDependOn: [domain, logging, ports]   # was: [..., circuitbreaker]
     canUse: [paho]
   ```
7. `make lint && make test && make lint-arch-check` must pass.

### Acceptance

- No file under `adapters/` imports `github.com/mariotoffia/gobridge/circuitbreaker`.
- `make lint-arch-check` passes with the tightened rule.
- `processors/circuitbreaker` continues to work (it lives in the
  `processors` component which still permits `circuitbreaker` because
  processors are an in-process transformation chain inside the hexagon).

---

## FIX-002: Ban `net/http` and `database/sql` from `ports/`

### Problem

`ports.HTTPMountable.Handler() http.Handler` leaks the stdlib `net/http`
type into the port layer. Any future adapter that wants to participate
in the same role is forced to also speak HTTP, even though the
*architectural* concept is "endpoint" or "mountable handler", not "HTTP".

### Outcome

- New port `ports.Endpoint` with a transport-neutral signature.
- `ports.HTTPMountable` deleted.
- The HTTP transport adapter implements `ports.Endpoint` and translates
  to `http.Handler` internally (in the adapter, where the leak is
  appropriate).
- `.go-arch-lint.yml` bans `net/http` and `database/sql` imports inside
  `ports/` via the `vendors:` section.

### Steps

1. Survey `ports.HTTPMountable` callers (`bridge.Builder.TransportHandlers`,
   `httpapi`, deployment registry). Catalogue what they actually need
   from the returned handler.
2. Define `ports.Endpoint`:

   ```go
   package ports

   // EndpointRequest is a transport-neutral HTTP-shaped request used
   // by adapters that expose endpoints. It carries only domain types
   // and primitives; it is decoupled from net/http so that future
   // delivery mechanisms (e.g., embedded RPC) can implement Endpoint
   // without dragging stdlib HTTP into ports.
   type EndpointRequest struct {
       Method string
       Path   string
       Header map[string][]string
       Body   []byte
   }

   type EndpointResponse struct {
       Status int
       Header map[string][]string
       Body   []byte
   }

   type Endpoint interface {
       Path() string
       Serve(req EndpointRequest) (EndpointResponse, error)
   }
   ```

   The shape is open to bikeshedding; the principle is "no `net/http`
   types cross this boundary."

3. Implement `ports.Endpoint` in the HTTP transport adapter as a thin
   wrapper around its existing `http.Handler` (the adapter still
   serves real HTTP — translation happens in the adapter).
4. Delete `ports.HTTPMountable`. Update `bridge.Builder.TransportHandlers`
   to scan for `ports.Endpoint` instead.
5. Edit `.go-arch-lint.yml` to add stdlib bans for `ports`:

   ```yaml
   vendors:
     stdlib_net_http:    { in: net/http }
     stdlib_database_sql: { in: database/sql }
     # …existing vendors

   deps:
     ports:
       mayDependOn: [domain]
       # ports has no canUse for stdlib_net_http / stdlib_database_sql
       # so importing them from ports/ will fail vendor lint.
   ```

   Note: `go-arch-lint` lists what each component CAN use; what is not
   listed is implicitly denied when `depOnAnyVendor: false` is set.
   Listing the stdlib paths in `vendors:` makes the bans visible to
   future maintainers reading the file.

6. Run `make lint-arch-check && make test`.

### Acceptance

- `grep -r "net/http" ports/` returns zero matches in non-test files.
- HTTP transport tests still pass.
- Bridge `TransportHandlers()` still returns endpoints (just typed as
  `ports.Endpoint`).

---

## FIX-003: Eliminate `bridge → config` and `httpapi → config` direct dependencies

### Problem

Today `bridge` imports `config` directly to read `*config.BridgeConfig`,
and `httpapi` imports `config` to expose admin endpoints over the
config DTOs. This makes `config` part of the application's stable API
surface. In strict Clean Architecture, application services should
consume *parsed inputs* through a port, not the parser package.

### Outcome

`config` is consumed only at the composition root and via ports.

- New port group: `ports.RuntimeBlueprint` describing the inputs
  `bridge.Builder` actually needs (sessions, receivers, senders,
  routes, stores, etc.) in port-language.
- `bridge.Builder.NewBuilder` accepts `ports.RuntimeBlueprint`, not
  `*config.BridgeConfig`. The composition root in `cmd/` and
  `deployment/` translates `*config.BridgeConfig` to a blueprint.
- New port group for HTTP admin: `ports.ConfigStore` with `Load`,
  `Save`, `Watch` returning blueprint-shaped data, not
  `*config.BridgeConfig`. `config.Manager` implements it.
- `bridge` and `httpapi` no longer import `config`.

### Steps

1. Inventory every field of `config.BridgeConfig` that `bridge.Builder`
   reads. Group into role-coherent types in `ports/`.
2. Inventory every config field that `httpapi/admin_config.go` and
   `httpapi/config_txn.go` expose. Define matching ports.
3. Implement adapter functions in `cmd/` and `deployment/` that
   convert `*config.BridgeConfig` to the blueprint and the config
   store.
4. Refactor `bridge.NewBuilder` signature.
5. Refactor `httpapi.New` to take a `ports.ConfigStore` instead of a
   `*config.Manager`.
6. Delete `config` from `bridge.mayDependOn` and `httpapi.mayDependOn`
   in `.go-arch-lint.yml`.
7. `make lint-arch-check && make test`.

### Cost warning

This is the largest task in the plan. It rewrites two of the central
public APIs (`bridge.NewBuilder`, `httpapi.New`) and rewrites the test
suites that depend on them. Per the **No Backward Compatibility** rule,
do this atomically — do not introduce a temporary parallel API.

### Acceptance

- `bridge/` and `httpapi/` source trees contain zero references to
  `github.com/mariotoffia/gobridge/config`.
- Every in-tree caller of `NewBuilder` and `httpapi.New` is updated.
- The role-based ports clearly express the application's input
  contract; `config` is one of several possible parsers.

---

## FIX-004: Split `domain/` into bounded-context sub-components

### Problem

`domain/` is currently a single 4,200-line package mixing routing,
persistence, and connectivity vocabulary (`RoutePolicy`, `OutboxRecord`,
`SessionMode` all live in the same package). Strict DDD demands
**bounded contexts** — separate packages with their own ubiquitous
language and explicit boundaries.

### Outcome

`domain/` becomes a small set of bounded-context sub-packages plus a
shared kernel:

```
domain/
├── shared/         # primitives crossing all contexts (BridgeError, IDs)
├── messaging/      # Envelope, Headers, TraceContext, ErrorClass
├── persistence/    # OutboxRecord, DLQEntry, DLQFilter, LeaseToken, LeaseInfo
├── routing/        # RoutePolicy, BackoffPolicy, DispatchPlan, DestinationBinding
├── connectivity/   # SessionMode, SessionPlan, SubscriptionPlan
└── clock/          # (unchanged)
```

`.go-arch-lint.yml` models each as a separate component. Cross-context
references must go through `domain/shared` or be translated by the
application layer. This catches accidental cross-context coupling
(e.g., a routing type reaching directly into a persistence type).

### Steps

1. Move types into the right context sub-package using `git mv` (file
   by file). Rename the package declarations.
2. Update import paths everywhere via a scripted replace, then `gofmt`.
3. Add components to `.go-arch-lint.yml`:

   ```yaml
   components:
     domain_shared:        { in: [domain] }
     domain_messaging:     { in: [domain/messaging, domain/messaging/**] }
     domain_persistence:   { in: [domain/persistence, domain/persistence/**] }
     domain_routing:       { in: [domain/routing, domain/routing/**] }
     domain_connectivity:  { in: [domain/connectivity, domain/connectivity/**] }
     # Replace the existing `domain` component above.

   deps:
     domain_shared:        { canUse: [_no_external_deps_] }
     domain_messaging:     { mayDependOn: [domain_shared] }
     domain_persistence:   { mayDependOn: [domain_shared] }
     domain_routing:       { mayDependOn: [domain_shared, domain_messaging] }
     domain_connectivity:  { mayDependOn: [domain_shared] }
   ```

   Note: `domain/clock` continues to be matched by `domain_shared`'s
   pattern because it lives under `domain/`. Adjust patterns if the
   layout differs.

4. Update every `mayDependOn` line that mentions `domain` to mention
   the specific sub-context(s) actually needed. For most components
   `[domain_shared, domain_messaging]` is sufficient.
5. Update `scripts/lint-arch-mapping-test.sh` to assert each
   sub-context maps to its expected component.
6. `make lint-arch-check && make test`.

### Cost warning

This is a large mechanical change but each step is reversible. Use
`git mv` for history preservation. Update one bounded context per
commit if needed.

### Acceptance

- `domain/` no longer has a single flat package; cross-context
  references are explicit.
- The lint mapping test verifies each sub-context.
- Code reads better: routing files no longer mention persistence
  primitives directly; they receive translated types from the
  application layer.

---

# Tier 2 — `golangci-lint` configuration

## FIX-005: Add `.golangci.yml`

### Problem

The repository runs `golangci-lint run ./...` with default linters
only. Architecture-relevant linters (`depguard`, `interfacebloat`,
`ireturn`, `forbidigo`, `wrapcheck`, `gochecknoglobals`, `gochecknoinits`,
`revive`) are off. They catch what `go-arch-lint` cannot:
file-level import bans, ISP enforcement, time-injection discipline,
DDD purity rules.

### Outcome

A `.golangci.yml` configured to strictly enforce DDD/Hex/Clean rules
that operate at file or symbol level (rather than at component level
where `go-arch-lint` already applies).

### Steps

Create `.golangci.yml`:

```yaml
version: "2"

linters:
  default: standard
  enable:
    - depguard
    - forbidigo
    - gochecknoglobals
    - gochecknoinits
    - interfacebloat
    - ireturn
    - revive
    - wrapcheck

  settings:
    interfacebloat:
      max: 5

    forbidigo:
      forbid:
        # Force time injection through domain/clock.
        - pattern: '^time\.Now$'
          msg: "use clock.Now via dependency injection (see domain/clock)"

    depguard:
      rules:
        # The domain layer must not depend on outer layers.
        domain:
          list-mode: lax
          files:
            - "**/domain/**/*.go"
            - "!$test"
          deny:
            - pkg: "github.com/mariotoffia/gobridge/ports"
              desc: "domain must not depend on ports"
            - pkg: "github.com/mariotoffia/gobridge/config"
              desc: "domain must not depend on config"
            - pkg: "github.com/mariotoffia/gobridge/runtime"
              desc: "domain must not depend on runtime"
            - pkg: "github.com/mariotoffia/gobridge/bridge"
              desc: "domain must not depend on bridge"

        # Ports must not leak technology types.
        ports:
          list-mode: lax
          files:
            - "**/ports/**/*.go"
            - "!$test"
          deny:
            - pkg: "net/http"
              desc: "ports must not return stdlib HTTP types; use ports.Endpoint instead"
            - pkg: "database/sql"
              desc: "ports must not expose stdlib SQL types"
            - pkg: "github.com/mariotoffia/gobridge/config"
              desc: "ports must not depend on config"
            - pkg: "github.com/mariotoffia/gobridge/bridge"
              desc: "ports must not depend on bridge"
            - pkg: "github.com/mariotoffia/gobridge/runtime"
              desc: "ports must not depend on runtime"

        # Adapters must not reach into composition or application layers.
        # Config-source adapters under adapters/*/config/* are exempted.
        adapters-no-bridge:
          list-mode: lax
          files:
            - "**/adapters/**/*.go"
            - "!**/adapters/*/config/**"
            - "!$test"
          deny:
            - pkg: "github.com/mariotoffia/gobridge/bridge"
              desc: "adapters must not depend on bridge (composition root)"
            - pkg: "github.com/mariotoffia/gobridge/runtime"
              desc: "adapters must not depend on runtime"
            - pkg: "github.com/mariotoffia/gobridge/config"
              desc: "only adapters/*/config/* may depend on config; other adapters consume ports specs"

        # Runtime is application; no config DTOs.
        runtime-no-config:
          list-mode: lax
          files:
            - "**/runtime/**/*.go"
            - "!$test"
          deny:
            - pkg: "github.com/mariotoffia/gobridge/config"
              desc: "runtime must not depend on config DTOs"
            - pkg: "github.com/mariotoffia/gobridge/bridge"
              desc: "runtime must not depend on bridge (composition root)"

    revive:
      rules:
        - name: max-public-structs
          arguments: [15]
        - name: cognitive-complexity
          arguments: [15]
        - name: cyclomatic
          arguments: [15]
        - name: unhandled-error
        - name: empty-block

  exclusions:
    rules:
      # gochecknoglobals applies only to inner-ring layers; cmd and
      # deployment may keep wiring globals.
      - linters: [gochecknoglobals, gochecknoinits]
        path: "^(cmd|deployment)/"
      - linters: [interfacebloat]
        # CredentialAdmin (5) and other coordination interfaces may
        # legitimately exceed 5; whitelist via path or rule.
        path: "ports/credentials.go"
```

### Verification

After landing FIX-001..004 the file's `depguard` rules should pass
with no exceptions. Until then, you may need to keep some `deny`
rules commented out (with a `// TODO(FIX-XXX): re-enable after
refactor`) and re-enable each as the corresponding refactor lands.

### Acceptance

- `make lint` passes with the new config.
- All listed linters are active (verify with `golangci-lint linters`).
- Removing the `domain` deny block and re-running shows the lint
  catches a deliberately injected import (sanity check).

---

# Tier 3 — Custom analyzers

These are small, maintainable Go programs that close two architectural
gaps no general-purpose linter handles. Each lives under
`scripts/<name>/` and is wired into the Makefile.

## FIX-006: Anti-Corruption Layer naming analyzer (3.1)

### Problem

When the SQS adapter receives a `types.Message` from the AWS SDK, it
maps the value into `domain.Envelope`. This mapping is the
hexagonal anti-corruption layer (ACL). Today the mapping is scattered
across multiple files (`receiver.go`, `headers.go`, `errors.go`).
Future maintainers cannot see the ACL boundary at a glance, and SDK
types can leak across the adapter without anyone noticing.

### Outcome

A custom `go/analysis` analyzer that flags any file inside
`adapters/*/transport/*` or `adapters/*/store/*` that imports a vendor
SDK package unless its filename matches `acl*.go` or it lives under an
`acl/` sub-directory.

### Steps

1. Create `scripts/aclcheck/main.go` and `scripts/aclcheck/analyzer.go`:

   ```go
   // Package aclcheck implements a go/analysis pass that enforces the
   // hexagonal anti-corruption layer naming convention.
   //
   // Inside an adapter package directory, the only files allowed to
   // import vendor SDK packages are:
   //
   //   * Files whose name matches acl*.go (e.g., acl_envelope.go).
   //   * Files in an acl/ sub-directory of the adapter package.
   //
   // The analyzer is configured with a list of "vendor SDK" import
   // path globs to enforce on. By default it checks the major SDK
   // roots used by this project (see vendorPatterns below).
   package aclcheck

   import (
       "go/ast"
       "path/filepath"
       "strings"

       "golang.org/x/tools/go/analysis"
   )

   var vendorPatterns = []string{
       "github.com/aws/aws-sdk-go-v2/",
       "github.com/Azure/",
       "github.com/eclipse/paho.golang/",
       "github.com/rabbitmq/amqp091-go",
       "modernc.org/sqlite",
       "github.com/fsnotify/fsnotify",
       "go.opentelemetry.io/",
   }

   var Analyzer = &analysis.Analyzer{
       Name: "aclcheck",
       Doc:  "checks that vendor SDK imports stay confined to ACL files in adapter packages",
       Run:  run,
   }

   func run(pass *analysis.Pass) (interface{}, error) {
       // Only check adapter packages.
       if !strings.Contains(pass.Pkg.Path(), "/adapters/") {
           return nil, nil
       }
       for _, f := range pass.Files {
           filename := pass.Fset.Position(f.Pos()).Filename
           if isACLLocation(filename) {
               continue
           }
           for _, imp := range f.Imports {
               path := strings.Trim(imp.Path.Value, `"`)
               if !isVendorSDK(path) {
                   continue
               }
               pass.Reportf(imp.Pos(),
                   "vendor SDK import %q is allowed only in ACL files (acl*.go) or acl/ directories; move the SDK boundary into an ACL", path)
           }
       }
       return nil, nil
   }

   func isACLLocation(filename string) bool {
       base := filepath.Base(filename)
       if strings.HasPrefix(base, "acl") || strings.HasPrefix(base, "acl_") {
           return true
       }
       dir := filepath.Base(filepath.Dir(filename))
       return dir == "acl"
   }

   func isVendorSDK(path string) bool {
       for _, pat := range vendorPatterns {
           if strings.HasPrefix(path, pat) {
               return true
           }
       }
       return false
   }
   ```

   And a `main.go`:

   ```go
   package main

   import (
       "golang.org/x/tools/go/analysis/singlechecker"
       "github.com/mariotoffia/gobridge/scripts/aclcheck"
   )

   func main() { singlechecker.Main(aclcheck.Analyzer) }
   ```

2. Refactor each transport adapter so that vendor types live in
   `acl_*.go` files. Typical pattern: an `acl_inbound.go` that
   converts SDK message → `domain.Envelope`, and an
   `acl_outbound.go` that converts `domain.Envelope` → SDK send call.

3. Add a Makefile target:

   ```make
   build-aclcheck:
   	@mkdir -p bin
   	go build -o bin/aclcheck ./scripts/aclcheck

   lint-acl: build-aclcheck
   	@echo "Checking adapter ACL boundary..."
   	@go vet -vettool=$(PWD)/bin/aclcheck ./adapters/...
   ```

4. Add `lint-acl` to `lint-arch-check` so CI enforces it.

### Acceptance

- `make lint-acl` passes after each adapter is refactored.
- A new SDK import in a non-ACL file fails the check with a clear
  message.

---

## FIX-007: Aggregate-root marker analyzer (3.2)

### Problem

Today there is no machine-readable way to know which `domain/` types
are aggregate roots. Anyone editing the domain can quietly add a
mutating method or a struct without invariant enforcement and the
aggregate concept erodes.

### Outcome

A custom `go/analysis` analyzer that enforces a simple convention:

- A type that has unexported state and exported mutator methods is
  considered an aggregate. Aggregate types MUST live in a file or
  package whose name signals the role:
  - File ends in `_aggregate.go`, OR
  - File has the comment `// Aggregate root: <name>` immediately above
    the type declaration.
- Aggregate types MUST expose at least one validation method
  (`Validate()` or `IsValid() error` or method with a name beginning
  in `Apply` / `Reject`) — invariants are explicit, not implicit.

The exact convention is open to discussion. Pick *one* and enforce it.

### Steps

1. Decide the convention. Recommended starting set:
   - File pattern: types matching the aggregate criteria below MUST
     live in a file ending `_aggregate.go`.
   - Aggregate criteria: the type is a struct, has at least one
     unexported field, and has at least one method with a non-pointer
     receiver returning the type (i.e., a "transition method").
   - Required methods: each aggregate must declare a `Validate() error`
     method. The analyzer fails if the type matches the criteria but
     no `Validate` method is found in the same package.

2. Create `scripts/aggcheck/main.go` and `scripts/aggcheck/analyzer.go`.
   The analyzer body:
   - Walks each file under `domain/`.
   - For each `*ast.TypeSpec` of struct kind, checks the criteria.
   - If criteria match and the file does not end in `_aggregate.go`,
     report.
   - If criteria match and no `Validate` method is found in the
     package, report.

3. First, identify candidate aggregates in the existing domain.
   Likely candidates: `Envelope` (with mutation through `Clone`),
   `OutboxRecord`, `BridgeError`. Decide which are aggregates and
   which are pure value objects (value objects are exempt from this
   check by definition).

4. Move the aggregates to `*_aggregate.go` files (`git mv`), add
   `Validate()` methods that codify the invariants currently scattered
   in code review comments.

5. Add a Makefile target `lint-aggregate` and include it in
   `lint-arch-check`:

   ```make
   build-aggcheck:
   	@mkdir -p bin
   	go build -o bin/aggcheck ./scripts/aggcheck

   lint-aggregate: build-aggcheck
   	@echo "Checking domain aggregate conventions..."
   	@go vet -vettool=$(PWD)/bin/aggcheck ./domain/...
   ```

### Acceptance

- Every domain aggregate lives in `*_aggregate.go` and has a
  `Validate()` method.
- Adding a new aggregate without naming and `Validate()` fails the
  check.
- Pure value objects (`LeaseToken`, `Tag`, `TraceContext`, etc.) are
  not flagged.

### Notes

This task is **design-flavoured**: the value comes from forcing
yourself to articulate invariants, not from the analyzer itself. Treat
the analyzer as a guardrail that prevents drift after the design work.

---

# Tier 4 — Advisory third-party tools wired into `make install`

## FIX-008: Add `modgraphviz`, `dupl`, `goconst`

### Problem

Two architectural-quality smells are easy to miss without tooling:

- **Duplicate logic** across packages suggests a missing aggregate or
  shared value object — `dupl` finds it.
- **Repeated string/numeric literals** suggest missing domain
  constants — `goconst` finds it.
- **Module dependency direction shifts** (e.g., a transport adapter
  starts depending on a store implementation) are hard to spot in a
  diff — `modgraphviz` produces a visual graph for review.

All three are installable via `go install` and run as advisory checks
(non-blocking, manually invoked). They live in `make install` so any
contributor has them available.

### Steps

1. Add to `make install`:

   ```make
   install: ## Install all development and CI tools
   	@echo "Installing development tools..."
   	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
   	go install golang.org/x/vuln/cmd/govulncheck@latest
   	go install github.com/icholy/gomajor@latest
   	go install github.com/psampaz/go-mod-outdated@latest
   	go install github.com/loov/goda@latest
   	go install github.com/fe3dback/go-arch-lint@latest
   	go install golang.org/x/exp/cmd/modgraphviz@latest
   	go install github.com/mibk/dupl@latest
   	go install github.com/jgautheron/goconst/cmd/goconst@latest
   ```

2. Add advisory targets:

   ```make
   .PHONY: arch-graph dupl-report goconst-report

   arch-graph: ## Render the module dep graph as SVG (requires graphviz dot)
   	@mkdir -p reports
   	@go mod graph | modgraphviz | dot -Tsvg -o reports/arch-graph.svg
   	@echo "Wrote reports/arch-graph.svg"

   dupl-report: ## Find duplicate code blocks (advisory)
   	@mkdir -p reports
   	@dupl -threshold 75 ./... > reports/dupl.log || true
   	@echo "Duplicate-code report at reports/dupl.log"

   goconst-report: ## Find repeated literals (advisory)
   	@mkdir -p reports
   	@goconst -min-occurrences 4 -min-length 5 ./... > reports/goconst.log || true
   	@echo "Repeated-literals report at reports/goconst.log"

   arch-quality: arch-graph dupl-report goconst-report ## Run all advisory architecture-quality reports
   ```

3. Document in `DEVELOPMENT.md` that these are advisory, run on demand,
   not part of CI.

### Why advisory, not blocking

Both `dupl` and `goconst` produce false positives easily (test
fixtures, HTTP method strings, similar boilerplate). Forcing them to
pass would push contributors toward over-abstraction — the opposite of
what good DDD wants. Treat them as **review aids**: someone reads
`reports/dupl.log` once a release and asks "is there a missing domain
concept here?"

`modgraphviz` is a visualization tool — it cannot pass or fail. It
makes structural changes legible during code review.

### Verification

```bash
make install
make arch-quality
ls -la reports/
```

Should produce `arch-graph.svg`, `dupl.log`, `goconst.log`.

### Acceptance

- All three tools installable through `make install`.
- All three Make targets work and write to `reports/`.
- `DEVELOPMENT.md` documents when to run them.

---

# Process — drift verification workflow

## FIX-009: Update `.claude/CLAUDE.md` with the verification workflow

The user-visible artifact: the project's `.claude/CLAUDE.md` already
has an **Architecture Policy Maintenance Contract**. Append a new
section that prescribes how to use every tool above to verify the
project hasn't drifted. The workflow is consumed both by humans and
by future Claude sessions reading the file.

### Outcome

A new section in `.claude/CLAUDE.md` titled **"Architecture Drift
Verification Workflow"** with three subsections:

- **Per-PR (mandatory)** — what every change must pass.
- **Per-release (recommended)** — advisory reports a human reviews.
- **Cross-checks** — what each tool catches and where it overlaps.

### Content

The section must contain at least the following:

1. **The single command that gates merge:**
   ```bash
   make check
   # = build + golangci-lint + lint-arch-check + lint-acl + lint-aggregate + test
   ```
   Failures here mean drift. Fix the code, do not relax the rules.

2. **What each layer catches** (table form):

   | Tool | Rule layer | Example violation it catches |
   |---|---|---|
   | `go-arch-lint` (component imports) | Component-to-component direction | adapters/aws/transport/sqs imports bridge |
   | `go-arch-lint` (vendor canUse) | External-dep allowlist | domain imports gopkg.in/yaml.v3 |
   | `go-arch-lint` (deepScan) | Structural-typing leaks | runtime function parameter is a bridge type |
   | `go-arch-lint mapping test` | Component taxonomy stays role-based | someone reintroduces a blanket adapters/** glob |
   | `golangci-lint depguard` | File-pattern import bans | a non-ACL adapter file imports vendor SDK (only after FIX-006) |
   | `golangci-lint interfacebloat` | ISP enforcement | a port grew to 8 methods |
   | `golangci-lint forbidigo` | Forbidden symbol use | code uses `time.Now()` instead of `clock.Now` |
   | `golangci-lint gochecknoglobals` | Domain purity | a global was added to domain |
   | `golangci-lint wrapcheck` | Boundary error wrapping | error returned from adapter is not wrapped |
   | `aclcheck` | Anti-corruption layer placement | SDK type appears in a non-acl_*.go file |
   | `aggcheck` | Aggregate convention | a domain aggregate lacks Validate() |
   | `dupl` (advisory) | Missing aggregate/value object | duplicated logic across two packages |
   | `goconst` (advisory) | Missing domain constants | the same magic string appears 5+ times |
   | `modgraphviz` (advisory) | Module dep direction shift | visible in arch-graph.svg diff |

3. **Failure-to-fix translation table** — the most important part:

   | Failure | First instinct |
   |---|---|
   | `lint-arch` fails | The code is wrong. Refactor, don't relax the rule. |
   | `depguard` denies an import | Same: the file is in the wrong location, or the dependency is in the wrong direction. |
   | `interfacebloat` flags a port | Split the interface; one of the methods belongs to a different role. |
   | `forbidigo` flags `time.Now()` | Inject `clock.Clock` and use `clk.Now()`. |
   | `aclcheck` flags an adapter file | Move the SDK-touching code into `acl_*.go`. |
   | `aggcheck` flags a domain type | The type is becoming an aggregate; rename file and add `Validate()`. |
   | `dupl` flags repeated logic | Look for a missing aggregate root or domain service before deduplicating with a helper. |
   | `goconst` flags repeated literal | Define a domain-meaningful constant, do not just `const x = "..."`. |

4. **When to run the advisory tools:**
   - Before opening a PR that adds a new transport/store adapter.
   - At each release cut.
   - When investigating a smell that lint can't pinpoint.

### Steps

1. Read `.claude/CLAUDE.md`.
2. Append the new section *after* the existing Architecture Policy
   Maintenance Contract.
3. Update `make check` once FIX-006 and FIX-007 land so it includes
   `lint-acl` and `lint-aggregate`.

### Acceptance

- `.claude/CLAUDE.md` contains the new workflow section.
- The "single command that gates merge" actually exists in `Makefile`.
- The failure-to-fix translation table aligns with the linters
  enabled in `.golangci.yml` and the analyzers built under
  `scripts/`.

---

# Implementation order

The plan is intentionally ordered so each task either prepares the
ground for the next or stands alone:

1. **FIX-001** — Lift circuitbreaker to a port. Smallest task, biggest
   architectural clarity gain.
2. **FIX-005** (initial pass) — Add `.golangci.yml` with most rules
   enabled but with `// TODO` exemptions for the layers that
   FIX-002..004 will fix. Lock in what we already know to be true.
3. **FIX-002** — Replace `ports.HTTPMountable` with `ports.Endpoint`.
4. **FIX-008** — Wire the advisory tools. Independent of FIX-002..004.
5. **FIX-006** — Build `aclcheck` and refactor adapters into ACL
   files. Lands cleanly only after FIX-002.
6. **FIX-003** — Eliminate `bridge → config` and `httpapi → config`.
   Largest refactor; do it on its own branch.
7. **FIX-005** (final pass) — Re-enable the deny rules that were
   commented out for FIX-003.
8. **FIX-004** — Split `domain/` into bounded contexts. Independent of
   the rest.
9. **FIX-007** — Build `aggcheck` and add `Validate()` methods to
   identified aggregates.
10. **FIX-009** — Add the drift-verification workflow to
    `.claude/CLAUDE.md`.

---

# Definition of done

The plan is fully delivered when:

- All 9 FIX-xxx tasks above are complete.
- `make check` passes — including the new `lint-acl` and
  `lint-aggregate` steps.
- `golangci-lint linters` shows the 8 new linters active with no
  silenced exemptions other than those documented in `.golangci.yml`.
- `make arch-quality` produces three reports under `reports/`.
- `.claude/CLAUDE.md` contains the drift-verification workflow.
- A trial violation in each tool (deliberately introduced and
  reverted) confirms detection.
