# Makefile for gobridge multi-module workspace
#
# This Makefile provides convenient commands for building, testing, and
# maintaining the multi-module Go workspace.

.PHONY: all build test test-integration test-long-running lint lint-fix lint-gofmt lint-go-vet lint-go lint-arch lint-arch-report lint-arch-mapping lint-arch-mapping-test lint-arch-check clean tidy sync help
.PHONY: install vulncheck update update-major outdated
.PHONY: hooks hooks-install hooks-uninstall
.PHONY: audit-timings audit-test-timings
.PHONY: arch-graph dupl-report goconst-report arch-quality
.PHONY: build-aclcheck lint-acl build-aggcheck lint-aggregate build-cfgshape lint-cfgshape build-registrychk lint-registrychk build-pluginsym lint-pluginsym

GOBRIDGE_GO_CACHE ?= /tmp/gobridge-go-build-cache
export GOCACHE ?= $(GOBRIDGE_GO_CACHE)

# Default target
all: build test

# ============================================================================
# Build targets
# ============================================================================

build: ## Build all modules
	@echo "Building all modules..."
	go build ./...

# ============================================================================
# Test targets
# ============================================================================

test: audit-timings audit-test-timings ## Run unit tests only (no Docker, integration tests skipped)
	@mkdir -p reports
	@echo "Running unit tests across all modules..."
	@echo "Report will be saved to: reports/test-unit.log"
	@bash -c 'for modfile in $$(find . -name go.mod -not -path "*/vendor/*" -not -path "*/tests/longrunning/*" | sort); do \
		dir=$$(dirname "$$modfile"); \
		echo "--- Testing $$dir ---"; \
		(cd "$$dir" && go test -short -race -timeout 120s ./...) || true; \
	done 2>&1 | tee reports/test-unit.log; \
	echo ""; \
	echo "========================================"; \
	echo "  Test Report: reports/test-unit.log"; \
	echo "========================================"; \
	failed=$$(grep -cE "^FAIL\s" reports/test-unit.log || true); \
	if [ "$$failed" -gt 0 ]; then \
		echo ""; \
		echo "FAILED tests:"; \
		grep -E "^--- FAIL:" reports/test-unit.log || true; \
		echo ""; \
		echo "FAILED packages ($$failed):"; \
		grep -E "^FAIL\s" reports/test-unit.log || true; \
		exit 1; \
	fi'

test-integration: audit-timings audit-test-timings ## Run all tests including integration (requires Docker)
	@mkdir -p reports
	@echo "Running all tests (unit + integration) across all modules..."
	@echo "Report will be saved to: reports/test-integration.log"
	@bash -c 'for modfile in $$(find . -name go.mod -not -path "*/vendor/*" -not -path "*/tests/longrunning/*" | sort); do \
		dir=$$(dirname "$$modfile"); \
		echo "--- Testing $$dir ---"; \
		(cd "$$dir" && AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test \
			go test -p 1 -race -timeout 600s -v ./...) || true; \
	done 2>&1 | tee reports/test-integration.log; \
	echo ""; \
	echo "========================================"; \
	echo "  Test Report: reports/test-integration.log"; \
	echo "========================================"; \
	failed=$$(grep -cE "^FAIL\s" reports/test-integration.log || true); \
	if [ "$$failed" -gt 0 ]; then \
		echo ""; \
		echo "FAILED tests:"; \
		grep -E "^--- FAIL:" reports/test-integration.log || true; \
		echo ""; \
		echo "FAILED packages ($$failed):"; \
		grep -E "^FAIL\s" reports/test-integration.log || true; \
		exit 1; \
	fi'

test-long-running: audit-timings audit-test-timings ## Run long-running stress tests (requires Docker, -tags=longrunning)
	@mkdir -p reports
	@echo "Running long-running stress tests..."
	@echo "Report will be saved to: reports/test-long-running.log"
	@bash -c 'set -o pipefail; AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test \
	GOBRIDGE_MQTT_MEMORY=256m GOBRIDGE_MQTT_CPUS=2.0 \
	GOBRIDGE_SQS_MEMORY=2g GOBRIDGE_SQS_CPUS=2.0 \
	GOBRIDGE_DDB_MEMORY=1g GOBRIDGE_DDB_CPUS=2.0 \
		go test -race -timeout 10800s -v -tags=longrunning ./tests/longrunning/... 2>&1 | tee reports/test-long-running.log; \
		rc=$$?; \
		echo ""; \
		echo "========================================"; \
		echo "  Test Report: reports/test-long-running.log"; \
		echo "========================================"; \
		if [ $$rc -ne 0 ]; then \
			echo ""; \
			echo "FAILED tests:"; \
			grep -E "^--- FAIL:" reports/test-long-running.log || true; \
			echo ""; \
			echo "FAILED packages:"; \
			grep -E "^FAIL\s" reports/test-long-running.log || true; \
		fi; \
		exit $$rc'

# ============================================================================
# Lint targets
# ============================================================================

lint: lint-arch-check lint-gofmt lint-go-vet lint-go lint-aggregate lint-acl lint-cfgshape lint-registrychk lint-pluginsym ## Run all static checks across the workspace

lint-go: ## Run golangci-lint across all workspace modules (uses .golangci.yml at the repo root)
	@echo "Running golangci-lint across all modules..."
	@bash -c 'set -e; mkdir -p "$(GOBRIDGE_GO_CACHE)"; export GOCACHE="$(GOBRIDGE_GO_CACHE)"; \
	for modfile in $$(find . -name go.mod -not -path "*/vendor/*" | sort); do \
		dir=$$(dirname "$$modfile"); \
		if [ -z "$$(cd "$$dir" && go list ./... 2>/dev/null)" ]; then \
			echo "--- Skipping $$dir (no default-tag packages) ---"; \
			continue; \
		fi; \
		echo "--- golangci-lint $$dir ---"; \
		(cd "$$dir" && golangci-lint run --timeout=5m ./...); \
	done'

lint-fix: ## Lint and auto-fix all workspace modules
	@echo "Linting with auto-fix..."
	@gofmt -w $$(git ls-files '*.go')

lint-gofmt: ## Check Go formatting across tracked Go files
	@echo "Checking Go formatting..."
	@FILES=$$(gofmt -l $$(git ls-files '*.go')); \
	if [ -n "$$FILES" ]; then \
		echo "$$FILES"; \
		echo ""; \
		echo "Go files need formatting. Run: make lint-fix"; \
		exit 1; \
	fi

lint-go-vet: ## Run go vet across all workspace modules
	@echo "Running go vet across all modules..."
	@bash -c 'set -e; mkdir -p "$(GOBRIDGE_GO_CACHE)"; export GOCACHE="$(GOBRIDGE_GO_CACHE)"; \
	for modfile in $$(find . -name go.mod -not -path "*/vendor/*" | sort); do \
		dir=$$(dirname "$$modfile"); \
		if [ -z "$$(cd "$$dir" && go list ./... 2>/dev/null)" ]; then \
			echo "--- Skipping $$dir (no default-tag packages) ---"; \
			continue; \
		fi; \
		echo "--- Vetting $$dir ---"; \
		(cd "$$dir" && go vet ./...); \
	done'

lint-arch: ## Check architecture dependencies (strict)
	@echo "Linting architecture..."
	go-arch-lint check --project-path . --max-warnings 1024 --output-color=false

lint-arch-report: ## Write a non-blocking architecture lint report
	@mkdir -p reports
	@echo "Linting architecture..."
	@bash -c 'set -o pipefail; go-arch-lint check --project-path . --max-warnings 1024 --output-color=false 2>&1 | tee reports/go-arch-lint.log; rc=$$?; \
		if [ $$rc -ne 0 ]; then \
			echo ""; \
			echo "Architecture warnings captured in reports/go-arch-lint.log"; \
			exit 0; \
		fi'
	@go-arch-lint graph --out reports/go-arch-lint-graph.svg

lint-arch-mapping: ## Show package-to-component mapping (debug aid)
	@echo "Resolving architecture component mapping..."
	@go-arch-lint mapping --project-path . --scheme grouped --output-color=false

lint-arch-check: lint-arch lint-arch-mapping-test ## Run strict lint and the regression mapping test
	@echo "Architecture lint and mapping test passed."

lint-arch-mapping-test: ## Verify key packages map to their expected lint components
	@echo "Verifying architecture component mapping..."
	@bash scripts/lint-arch-mapping-test.sh

build-aclcheck: ## Build the aclcheck custom analyzer
	@mkdir -p bin
	@cd scripts/aclcheck && go build -o $(PWD)/bin/aclcheck ./...

# lint-acl is enforcing: it runs aclcheck across every adapter module
# and fails the build on any vendor SDK import outside an acl_*.go file
# (or acl/ sub-directory). The aggregated log under reports/aclcheck.log
# is preserved as a post-mortem aid; the per-module `go vet` invocation
# is the gate.
lint-acl: build-aclcheck ## Run aclcheck (enforcing) — fails on any non-ACL SDK import
	@mkdir -p reports
	@echo "Running aclcheck..."
	@bash -c 'set -eo pipefail; : > reports/aclcheck.log; for modfile in $$(find ./adapters -name go.mod -not -path "*/vendor/*" | sort); do \
		dir=$$(dirname "$$modfile"); \
		if [ -z "$$(cd "$$dir" && go list ./... 2>/dev/null)" ]; then continue; fi; \
		echo "--- aclcheck $$dir ---" | tee -a reports/aclcheck.log; \
		(cd "$$dir" && go vet -vettool=$(PWD)/bin/aclcheck ./... 2>&1) | tee -a $(PWD)/reports/aclcheck.log; \
	done'

build-aggcheck: ## Build the aggcheck custom analyzer
	@mkdir -p bin
	@cd scripts/aggcheck && go build -o $(PWD)/bin/aggcheck ./...

# lint-aggregate is enforcing: aggregate-like types in domain/ must
# live in *_aggregate.go files and declare a Validate() method. The
# convention is opt-in — pure value objects and types whose mutation
# is via pointer receiver are NOT aggregates and are exempt.
lint-aggregate: build-aggcheck ## Enforce aggregate-root naming convention in domain/
	@echo "Checking domain aggregate conventions..."
	@go vet -vettool=$(PWD)/bin/aggcheck ./domain/...

build-cfgshape: ## Build the cfgshape custom analyzer
	@mkdir -p bin
	@cd scripts/cfgshape && go build -o $(PWD)/bin/cfgshape ./...

# lint-cfgshape is enforcing: it runs cfgshape across the root module
# and every adapter module to enforce typed pluggable config shapes
# (see FIX-003). The aggregated log under reports/cfgshape.log is
# preserved as a post-mortem aid; the per-module `go vet` invocation
# is the gate. Script and test-only modules (scripts/, tests/,
# testutil/) are excluded to avoid false positives — the rule targets
# inner-ring + adapter packages only.
lint-cfgshape: build-cfgshape ## Enforce typed pluggable config shapes
	@mkdir -p reports
	@echo "Running cfgshape..."
	@bash -c 'set -eo pipefail; : > reports/cfgshape.log; for modfile in $$(find . -name go.mod -not -path "*/vendor/*" -not -path "./scripts/*" -not -path "./tests/*" -not -path "./testutil/*" | sort); do \
		dir=$$(dirname "$$modfile"); \
		if [ -z "$$(cd "$$dir" && go list ./... 2>/dev/null)" ]; then continue; fi; \
		echo "--- cfgshape $$dir ---" | tee -a reports/cfgshape.log; \
		(cd "$$dir" && go vet -vettool=$(PWD)/bin/cfgshape ./... 2>&1) | tee -a $(PWD)/reports/cfgshape.log; \
	done'

build-registrychk: ## Build the registrychk tool
	@mkdir -p bin
	@cd scripts/registrychk && go build -o $(PWD)/bin/registrychk ./...

# lint-registrychk is enforcing: every kind in ports.DefaultRegistry
# that is AWS-deployable has both a bridgecfg builder (With<Kind>*)
# and a grants helper (cdk/constructs/internal/grants/<kind>.go).
# Pure non-AWS kinds (azure.*, amqp.*) are skipped — the file-based
# config CDK only deploys to AWS.
lint-registrychk: build-registrychk ## Enforce CDK builder + grants coverage for all AWS-deployable plugin kinds
	@echo "Running registrychk..."
	@$(PWD)/bin/registrychk

build-pluginsym: ## Build the pluginsym tool
	@mkdir -p bin
	@cd scripts/pluginsym && go build -o $(PWD)/bin/pluginsym ./...

# lint-pluginsym is enforcing: every kind registered into the per-process
# *ports.Registry by the canonical composition root has a corresponding
# wired factory (Supervisor.RegisterTransport / RegisterStoreFactory or
# Builder.RegisterTransportFactory / RegisterStoreFactory) and vice-versa.
# Aliases (e.g. aws.sqs / sqs, mqtt.paho / mqtt) are collapsed via the
# curated aliasMap; a canonical group is satisfied if any alias is wired.
lint-pluginsym: build-pluginsym ## Enforce plugin-registry symmetry between decoders and wired factories
	@echo "Running pluginsym..."
	@$(PWD)/bin/pluginsym

# ============================================================================
# Maintenance targets
# ============================================================================

tidy: ## Sync workspace and tidy all module dependencies
	@echo "Syncing workspace..."
	go work sync
	@echo "Tidying all modules..."
	@find . -name go.mod -not -path '*/vendor/*' -execdir sh -c 'echo "Tidying $$(pwd)..." && go mod tidy' \;

sync: tidy ## Alias for tidy (workspace sync is included)

update: ## Update all dependencies to latest minor/patch versions
	@find . -name go.mod -not -path '*/vendor/*' -not -path '*/legacy/*' \
		-execdir sh -c 'echo "Updating $$(pwd)..." && go get -u ./... && go mod tidy' \;
	@$(MAKE) tidy

update-major: ## Show available major version upgrades (requires gomajor)
	@find . -name go.mod -not -path '*/vendor/*' -not -path '*/legacy/*' \
		-execdir sh -c 'echo "=== Major versions in $$(pwd) ===" && gomajor list' \;

outdated: ## Show outdated direct dependencies (requires go-mod-outdated)
	@find . -name go.mod -not -path '*/vendor/*' -not -path '*/legacy/*' \
		-execdir sh -c 'echo "=== Outdated in $$(pwd) ===" && go list -m -u -json all | go-mod-outdated -direct -update' \;

vulncheck: ## Check all modules for known vulnerabilities (requires govulncheck)
	@echo "Running vulnerability check..."
	@find . -name go.mod -not -path '*/vendor/*' -not -path '*/legacy/*' \
		-execdir sh -c 'echo "=== Checking $$(pwd) ===" && govulncheck ./...' \;

clean: ## Clean build cache and test cache
	@echo "Cleaning build cache..."
	go clean -cache -testcache ./...

# ============================================================================
# Development helpers
# ============================================================================

install: ## Install all development and CI tools
	@echo "Installing development tools..."
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest
	go install github.com/icholy/gomajor@latest
	go install github.com/psampaz/go-mod-outdated@latest
	go install github.com/loov/goda@latest
	go install github.com/fe3dback/go-arch-lint@latest
	go install github.com/mibk/dupl@latest
	go install github.com/jgautheron/goconst/cmd/goconst@latest

check: build lint lint-arch-check test audit-timings audit-test-timings ## Run full CI check (no Docker, integration skipped)

check-all: build lint lint-arch-check test-integration audit-timings audit-test-timings ## Run full CI check including integration (Docker required)

# ============================================================================
# Architecture-quality reports (advisory, non-blocking)
#
# These targets produce review aids — they are not gates. Forcing them
# to pass would push contributors toward over-abstraction (the opposite
# of what good DDD wants). Run them at release time or when
# investigating a smell that lint cannot pinpoint.
# ============================================================================

arch-graph: ## Dump the workspace module dep graph as text (LLM/grep-friendly)
	@mkdir -p reports
	@echo "Dumping module dep graph..."
	@go mod graph > reports/arch-graph.txt
	@echo "Wrote reports/arch-graph.txt ($$(wc -l < reports/arch-graph.txt | tr -d ' ') edges)"
	@echo "Inspect with: grep '^github.com/mariotoffia/gobridge ' reports/arch-graph.txt"

dupl-report: ## Find duplicate code blocks across the workspace (advisory)
	@mkdir -p reports
	@echo "Scanning for duplicate code (threshold 75 tokens)..."
	@dupl -threshold 75 ./... > reports/dupl.log || true
	@echo "Duplicate-code report at reports/dupl.log"

goconst-report: ## Find repeated string/numeric literals (advisory)
	@mkdir -p reports
	@echo "Scanning for repeated literals (>=4 occurrences, >=5 chars)..."
	@goconst -min-occurrences 4 -min-length 5 ./... > reports/goconst.log || true
	@echo "Repeated-literals report at reports/goconst.log"

arch-quality: arch-graph dupl-report goconst-report ## Run all advisory architecture-quality reports
	@echo "Architecture-quality reports written under reports/"

# ============================================================================
# Audit targets
# ============================================================================

TIMING_DIRS := adapters runtime bridge processors circuitbreaker httpapi

audit-timings: ## Check for unauthorized timing calls in production code
	@echo "Checking for unauthorized timing calls..."
	@PATTERNS=$$(grep -v '^#' audit/timing-allowlist.txt | grep -v '^$$' || true); \
	VIOLATIONS=$$(rg --no-heading -n -g '!*_test.go' -g '!testutil/**' \
		'time\.(Sleep|After|NewTicker|NewTimer|Tick)\(' \
		$(TIMING_DIRS) 2>/dev/null \
		| { if [ -n "$$PATTERNS" ]; then grep -v -F "$$PATTERNS"; else cat; fi; } \
		|| true); \
	if [ -n "$$VIOLATIONS" ]; then \
		echo "$$VIOLATIONS"; \
		COUNT=$$(echo "$$VIOLATIONS" | wc -l | tr -d ' '); \
		echo ""; \
		echo "$$COUNT unauthorized timing call(s) found."; \
		echo "Add a justified entry (# CLASS: reason + file:line: pattern)"; \
		echo "to audit/timing-allowlist.txt — see header for format."; \
		exit 1; \
	else \
		echo "All timing calls are authorized."; \
	fi

audit-test-timings: ## Check for new time.Sleep calls in test code
	@echo "Checking for new time.Sleep calls in tests..."
	@VIOLATIONS=$$(rg --no-heading -n -g '*_test.go' -g '!testutil/wait/*' \
		'time\.Sleep\(' . \
		| sort \
		| grep -v -F -f audit/test-timing-allowlist.txt); \
	if [ -n "$$VIOLATIONS" ]; then \
		echo "$$VIOLATIONS"; \
		COUNT=$$(echo "$$VIOLATIONS" | wc -l | tr -d ' '); \
		echo ""; \
		echo "$$COUNT new time.Sleep call(s) in tests."; \
		echo "Remove the sleep, or (with justification) add the line to"; \
		echo "audit/test-timing-allowlist.txt — annotate with // CLASS: reason."; \
		exit 1; \
	else \
		echo "No new test timing violations."; \
	fi

# ============================================================================
# Git hooks
# ============================================================================

hooks: hooks-install ## Alias for hooks-install

hooks-install: ## Install git pre-commit hooks (symlinks scripts/hooks/ into .git/hooks/)
	@echo "Installing git hooks..."
	@ln -sf ../../scripts/hooks/check-binaries.sh .git/hooks/pre-commit
	@echo "Installed pre-commit hook: check-binaries"

hooks-uninstall: ## Remove installed git hooks
	@echo "Removing git hooks..."
	@rm -f .git/hooks/pre-commit
	@echo "Git hooks removed."

# ============================================================================
# Help
# ============================================================================

help: ## Show this help
	@echo "gobridge - Multi-module Go workspace"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
