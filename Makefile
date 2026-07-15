# Makefile for gobridge multi-module workspace
#
# This Makefile provides convenient commands for building, testing, and
# maintaining the multi-module Go workspace.

.PHONY: all build test test-cdk-norace test-integration test-long-running lint lint-fix check check-all clean tidy sync help
.PHONY: install vulncheck update update-major outdated
.PHONY: hooks hooks-install hooks-uninstall
.PHONY: audit-timings audit-test-timings
.PHONY: arch-graph dupl-report goconst-report
.PHONY: build-aclcheck build-aggcheck build-cfgshape build-registrychk build-pluginsym
.PHONY: docker-build update-seeder-image

GOBRIDGE_GO_CACHE ?= /tmp/gobridge-go-build-cache
export GOCACHE ?= $(GOBRIDGE_GO_CACHE)

# Container image coordinates (override on the command line, e.g.
# `make docker-build IMAGE=ghcr.io/mariotoffia/gobridge IMAGE_TAG=v1.2.3`).
IMAGE      ?= ghcr.io/mariotoffia/gobridge
IMAGE_TAG  ?= dev
GIT_SHA    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

# Default target
all: build test

# ============================================================================
# Build targets
# ============================================================================

build: ## Build all modules
	@echo "Building all modules..."
	go build ./...

# ============================================================================
# Container image
# ============================================================================

docker-build: ## Build the production runtime image (gobridge-filebased, no push)
	@echo "Building $(IMAGE):$(IMAGE_TAG) ..."
	docker build \
		--build-arg VERSION=$(IMAGE_TAG) \
		--build-arg GIT_SHA=$(GIT_SHA) \
		-t $(IMAGE):$(IMAGE_TAG) .

update-seeder-image: ## Refresh the pinned seeder (aws-cli) digest and commit-ready image.txt
	$(MAKE) -C deployment/aws-filebased-config update-seeder-image

# ============================================================================
# Test targets
# ============================================================================

test: audit-timings audit-test-timings ## Run unit tests only (no Docker, integration tests skipped)
	@mkdir -p reports
	@echo "Running unit tests across all modules..."
	@echo "Report will be saved to: reports/test-unit.log"
	@bash -c 'set -o pipefail; { rc=0; for modfile in $$(find . -name go.mod -not -path "*/vendor/*" -not -path "*/tests/longrunning/*" | sort); do \
		dir=$$(dirname "$$modfile"); \
		echo "--- Testing $$dir ---"; \
		(cd "$$dir" && go test -count=1 -short -race -timeout 120s ./...) || rc=$$?; \
	done; exit $$rc; } 2>&1 | tee reports/test-unit.log; \
	rc=$$?; \
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
	fi; \
	exit $$rc'

test-cdk-norace: ## Run CDK assertions excluded from race builds
	@echo "Running non-race CDK assertion suites..."
	@cd deployment/aws-filebased-config/cdk && go test -count=1 -timeout 120s \
		./constructs/internal/grants ./constructs/internal/gobridgebase \
		./constructs/gobridgedynamodbha ./constructs/gobridgealarms

test-integration: audit-timings audit-test-timings ## Run all tests including integration (requires Docker)
	@mkdir -p reports
	@echo "Running all tests (unit + integration) across all modules..."
	@echo "Report will be saved to: reports/test-integration.log"
	@bash -c 'set -o pipefail; { rc=0; for modfile in $$(find . -name go.mod -not -path "*/vendor/*" -not -path "*/tests/longrunning/*" | sort); do \
		dir=$$(dirname "$$modfile"); \
		echo "--- Testing $$dir ---"; \
		(cd "$$dir" && AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test \
			go test -count=1 -p 1 -race -timeout 600s -v ./...) || rc=$$?; \
	done; exit $$rc; } 2>&1 | tee reports/test-integration.log; \
	rc=$$?; \
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
	fi; \
	exit $$rc'

test-long-running: audit-timings audit-test-timings ## Run long-running stress tests (requires Docker, -tags=longrunning)
	@mkdir -p reports
	@echo "Running long-running stress tests..."
	@echo "Report will be saved to: reports/test-long-running.log"
	@bash -c 'set -o pipefail; AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test \
	GOBRIDGE_MQTT_MEMORY=256m GOBRIDGE_MQTT_CPUS=2.0 \
	GOBRIDGE_SQS_MEMORY=2g GOBRIDGE_SQS_CPUS=2.0 \
	GOBRIDGE_DDB_MEMORY=1g GOBRIDGE_DDB_CPUS=2.0 \
		go test -count=1 -race -timeout 10800s -v -tags=longrunning ./tests/longrunning/... 2>&1 | tee reports/test-long-running.log; \
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
# Lint
#
# Single entry point. Runs every static check on the workspace and writes
# one report per checker under reports/. Used by CI and `make check`.
# Fail-fast: the first failing checker stops the build; subsequent reports
# are from the previous successful run. Read reports/<latest-step>.log
# when something is red.
#
# Auto-format escape hatch: `make lint-fix` runs gofmt on all tracked files.
# ============================================================================

lint: build-aclcheck build-aggcheck build-cfgshape build-registrychk build-pluginsym ## Run every static check (arch, gofmt, go vet, golangci-lint, aggcheck, aclcheck, cfgshape, registrychk, pluginsym); writes reports/*
	@mkdir -p reports
	@echo "=== Architecture lint ==="
	@bash -c 'set -o pipefail; go-arch-lint check --project-path . --max-warnings 1024 --output-color=false 2>&1 | tee reports/go-arch-lint.log'
	@go-arch-lint graph --out reports/go-arch-lint-graph.svg
	@bash -c 'set -o pipefail; bash scripts/lint-arch-mapping-test.sh 2>&1 | tee reports/arch-mapping.log'
	@echo "=== x-bridge header governance ==="
	@bash -c 'set -o pipefail; bash scripts/lint-xbridge-headers.sh --self-test 2>&1 | tee reports/xbridge-headers.log'
	@echo "=== gofmt ==="
	@FILES=$$(gofmt -l $$(git ls-files '*.go')); \
	echo "$$FILES" > reports/gofmt.log; \
	if [ -n "$$FILES" ]; then \
		echo "$$FILES"; \
		echo ""; \
		echo "Go files need formatting (see reports/gofmt.log). Run: make lint-fix"; \
		exit 1; \
	fi
	@echo "=== go vet ==="
	@bash -c 'set -eo pipefail; mkdir -p "$(GOBRIDGE_GO_CACHE)"; export GOCACHE="$(GOBRIDGE_GO_CACHE)"; : > reports/go-vet.log; \
	for modfile in $$(find . -name go.mod -not -path "*/vendor/*" | sort); do \
		dir=$$(dirname "$$modfile"); \
		if [ -z "$$(cd "$$dir" && go list ./... 2>/dev/null)" ]; then \
			echo "--- Skipping $$dir (no default-tag packages) ---" | tee -a reports/go-vet.log; \
			continue; \
		fi; \
		echo "--- Vetting $$dir ---" | tee -a reports/go-vet.log; \
		(cd "$$dir" && go vet ./... 2>&1) | tee -a $(PWD)/reports/go-vet.log; \
	done'
	@echo "=== golangci-lint ==="
	@bash -c 'major=$$(golangci-lint version 2>/dev/null | sed -nE "s/.*version v?([0-9]+).*/\1/p" | head -1); \
	if [ "$$major" != "2" ]; then \
		echo "ERROR: .golangci.yml is schema v2 but installed golangci-lint is not v2 (found: $$(golangci-lint version 2>&1 | head -1))."; \
		echo "Run: make install"; \
		exit 1; \
	fi'
	@bash -c 'set -eo pipefail; mkdir -p "$(GOBRIDGE_GO_CACHE)"; export GOCACHE="$(GOBRIDGE_GO_CACHE)"; : > reports/golangci.log; \
	for modfile in $$(find . -name go.mod -not -path "*/vendor/*" | sort); do \
		dir=$$(dirname "$$modfile"); \
		if [ -z "$$(cd "$$dir" && go list ./... 2>/dev/null)" ]; then \
			echo "--- Skipping $$dir (no default-tag packages) ---" | tee -a reports/golangci.log; \
			continue; \
		fi; \
		echo "--- golangci-lint $$dir ---" | tee -a reports/golangci.log; \
		(cd "$$dir" && golangci-lint run --timeout=5m ./... 2>&1) | tee -a $(PWD)/reports/golangci.log; \
	done'
	@echo "=== aggcheck (domain aggregate convention) ==="
	@bash -c 'set -o pipefail; go vet -vettool=$(PWD)/bin/aggcheck ./domain/... 2>&1 | tee reports/aggcheck.log'
	@echo "=== aclcheck (vendor SDK only via ACL files) ==="
	@bash -c 'set -eo pipefail; : > reports/aclcheck.log; \
	for modfile in $$(find ./adapters -name go.mod -not -path "*/vendor/*" | sort); do \
		dir=$$(dirname "$$modfile"); \
		if [ -z "$$(cd "$$dir" && go list ./... 2>/dev/null)" ]; then continue; fi; \
		echo "--- aclcheck $$dir ---" | tee -a reports/aclcheck.log; \
		(cd "$$dir" && go vet -vettool=$(PWD)/bin/aclcheck ./... 2>&1) | tee -a $(PWD)/reports/aclcheck.log; \
	done'
	@echo "=== cfgshape (typed plugin config) ==="
	@bash -c 'set -eo pipefail; : > reports/cfgshape.log; \
	for modfile in $$(find . -name go.mod -not -path "*/vendor/*" -not -path "./scripts/*" -not -path "./tests/*" -not -path "./testutil/*" | sort); do \
		dir=$$(dirname "$$modfile"); \
		if [ -z "$$(cd "$$dir" && go list ./... 2>/dev/null)" ]; then continue; fi; \
		echo "--- cfgshape $$dir ---" | tee -a reports/cfgshape.log; \
		(cd "$$dir" && go vet -vettool=$(PWD)/bin/cfgshape ./... 2>&1) | tee -a $(PWD)/reports/cfgshape.log; \
	done'
	@echo "=== registrychk (CDK builder + grants coverage) ==="
	@bash -c 'set -o pipefail; $(PWD)/bin/registrychk 2>&1 | tee reports/registrychk.log'
	@echo "=== pluginsym (registry symmetry) ==="
	@bash -c 'set -o pipefail; $(PWD)/bin/pluginsym 2>&1 | tee reports/pluginsym.log'
	@echo "=== Module graph (advisory) ==="
	@go mod graph > reports/arch-graph.txt
	@echo "reports/arch-graph.txt — $$(wc -l < reports/arch-graph.txt | tr -d ' ') edges"
	@echo "=== Duplicate scan (advisory) ==="
	@dupl -threshold 75 ./... > reports/dupl.log 2>&1 || true
	@echo "reports/dupl.log — $$(wc -l < reports/dupl.log | tr -d ' ') lines"
	@echo "=== Repeated literals (advisory) ==="
	@goconst -min-occurrences 4 -min-length 5 ./... > reports/goconst.log 2>&1 || true
	@echo "reports/goconst.log — $$(wc -l < reports/goconst.log | tr -d ' ') lines"
	@echo ""
	@echo "============================================================"
	@echo "  Lint passed."
	@echo "  Blocking reports:"
	@echo "    reports/go-arch-lint.log         reports/go-arch-lint-graph.svg"
	@echo "    reports/arch-mapping.log         reports/gofmt.log"
	@echo "    reports/go-vet.log               reports/golangci.log"
	@echo "    reports/aggcheck.log             reports/aclcheck.log"
	@echo "    reports/cfgshape.log             reports/registrychk.log"
	@echo "    reports/pluginsym.log"
	@echo "  Advisory reports (review aids; never fail the build):"
	@echo "    reports/arch-graph.txt           reports/dupl.log"
	@echo "    reports/goconst.log"
	@echo "============================================================"

lint-fix: ## Auto-format all tracked Go files with gofmt
	@echo "Linting with auto-fix..."
	@gofmt -w $$(git ls-files '*.go')

build-aclcheck: ## Build the aclcheck custom analyzer (vendor SDKs only via acl_*.go / acl/)
	@mkdir -p bin
	@cd scripts/aclcheck && go build -o $(PWD)/bin/aclcheck ./...

build-aggcheck: ## Build the aggcheck custom analyzer (aggregate-root convention in domain/)
	@mkdir -p bin
	@cd scripts/aggcheck && go build -o $(PWD)/bin/aggcheck ./...

build-cfgshape: ## Build the cfgshape custom analyzer (typed ports.PluginConfig)
	@mkdir -p bin
	@cd scripts/cfgshape && go build -o $(PWD)/bin/cfgshape ./...

build-registrychk: ## Build the registrychk tool (CDK builder + grants coverage)
	@mkdir -p bin
	@cd scripts/registrychk && go build -o $(PWD)/bin/registrychk ./...

build-pluginsym: ## Build the pluginsym tool (decoder ↔ wired factory symmetry)
	@mkdir -p bin
	@cd scripts/pluginsym && go build -o $(PWD)/bin/pluginsym ./...

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
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
	go install golang.org/x/vuln/cmd/govulncheck@latest
	go install github.com/icholy/gomajor@latest
	go install github.com/psampaz/go-mod-outdated@latest
	go install github.com/loov/goda@latest
	go install github.com/fe3dback/go-arch-lint@v1.15.0
	go install github.com/mibk/dupl@v1.1.0
	go install github.com/jgautheron/goconst/cmd/goconst@v1.10.2

check: build lint test ## Run full CI check (no Docker, integration skipped) — lint covers arch-check + analyzers; test runs timing audits

check-all: build lint test-integration ## Run full CI check including integration (Docker required) — lint covers arch-check + analyzers; test-integration runs timing audits

# ============================================================================
# Advisory report sub-targets
#
# These also run as the last three stages of `make lint`. Provided as
# standalone targets for ad-hoc invocation without running the full
# lint suite.
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
