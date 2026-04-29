# Makefile for gobridge multi-module workspace
#
# This Makefile provides convenient commands for building, testing, and
# maintaining the multi-module Go workspace.

.PHONY: all build test test-integration test-long-running lint lint-fix lint-gofmt lint-go-vet lint-arch lint-arch-report lint-arch-mapping lint-arch-mapping-test lint-arch-check clean tidy sync help
.PHONY: build-core build-mqtt build-aws build-azure
.PHONY: install vulncheck update update-major outdated
.PHONY: docker-up docker-down docker-clean
.PHONY: hooks hooks-install hooks-uninstall
.PHONY: audit-timings audit-test-timings

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

build-core: ## Build core module only
	@echo "Building core module..."
	go build ./bridge/...

build-mqtt: ## Build MQTT module only
	@echo "Building MQTT module..."
	cd adapters/mqtt/transport/paho && go build ./...

build-aws: ## Build AWS module only
	@echo "Building AWS module..."
	cd adapters/aws/transport/sqs && go build ./...

build-azure: ## Build Azure module only
	@echo "Building Azure module..."
	cd adapters/azure/transport/servicebus && go build ./...

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

lint: lint-arch-check lint-gofmt lint-go-vet ## Run all static checks across the workspace

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

lint-arch-mapping: ## Show package-to-component mapping (debug aid)
	@echo "Resolving architecture component mapping..."
	@go-arch-lint mapping --project-path . --scheme grouped --output-color=false

lint-arch-check: lint-arch lint-arch-mapping-test ## Run strict lint and the regression mapping test
	@echo "Architecture lint and mapping test passed."

lint-arch-mapping-test: ## Verify key packages map to their expected lint components
	@echo "Verifying architecture component mapping..."
	@bash scripts/lint-arch-mapping-test.sh

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

check: build lint lint-arch-check test audit-timings audit-test-timings ## Run full CI check (no Docker, integration skipped)

check-all: build lint lint-arch-check test-integration audit-timings audit-test-timings ## Run full CI check including integration (Docker required)

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
# Docker test containers
# ============================================================================

docker-up: ## Start persistent test containers for local development
	@echo "Starting test containers..."
	@cat /tmp/gobridge-mqtt.conf 2>/dev/null || printf 'listener 1883 0.0.0.0\nprotocol mqtt\nallow_anonymous true\npersistence false\nlog_dest stdout\n' > /tmp/gobridge-mqtt.conf
	-docker rm -f gobridge-ddb gobridge-sqs gobridge-mqtt 2>/dev/null
	docker run -d --name gobridge-ddb -p 127.0.0.1:8000:8000 amazon/dynamodb-local:latest -jar DynamoDBLocal.jar -sharedDb -inMemory
	docker run -d --name gobridge-sqs -p 127.0.0.1:9324:9324 softwaremill/elasticmq-native:latest
	docker run -d --name gobridge-mqtt -p 127.0.0.1:1883:1883 -v /tmp/gobridge-mqtt.conf:/mosquitto/config/mosquitto.conf:ro eclipse-mosquitto:latest
	@echo "Waiting for containers..."
	@sleep 3
	@echo "Containers ready. Run tests with:"
	@echo "  DYNAMODB_ENDPOINT=http://127.0.0.1:8000 SQS_ENDPOINT=http://127.0.0.1:9324 MQTT_BROKER_URL=tcp://127.0.0.1:1883 make test-integration"

docker-down: ## Stop and remove all gobridge test containers
	@echo "Stopping test containers..."
	-docker rm -f gobridge-ddb gobridge-sqs gobridge-mqtt 2>/dev/null

docker-clean: ## Remove ALL orphaned gobridge containers from any test run
	@echo "Cleaning orphaned containers..."
	-docker rm -f $$(docker ps -aq --filter name=gobridge-asblocal-) 2>/dev/null
	-docker network rm $$(docker network ls -q --filter name=gobridge-asbnet-) 2>/dev/null
	-docker rm -f $$(docker ps -aq --filter name=gobridge-ddblocal-) 2>/dev/null
	-docker rm -f $$(docker ps -aq --filter name=gobridge-sqslocal-) 2>/dev/null
	-docker rm -f $$(docker ps -aq --filter name=gobridge-s3local-) 2>/dev/null
	-docker rm -f $$(docker ps -aq --filter name=gobridge-localstack-) 2>/dev/null
	-docker rm -f $$(docker ps -aq --filter name=gobridge-mqtt-) 2>/dev/null
	-docker rm -f gobridge-ddb gobridge-sqs gobridge-mqtt 2>/dev/null
	@echo "Done."

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
