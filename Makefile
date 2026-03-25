# Makefile for gobridge multi-module workspace
#
# This Makefile provides convenient commands for building, testing, and
# maintaining the multi-module Go workspace.

.PHONY: all build test test-integration lint lint-fix clean tidy sync help
.PHONY: build-core build-mqtt build-aws build-azure
.PHONY: install vulncheck update update-major outdated
.PHONY: docker-up docker-down docker-clean

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
	cd transport/mqtt && go build ./...

build-aws: ## Build AWS module only
	@echo "Building AWS module..."
	cd transport/aws && go build ./...

build-azure: ## Build Azure module only
	@echo "Building Azure module..."
	cd transport/azure && go build ./...

# ============================================================================
# Test targets
# ============================================================================

test: ## Run unit tests only (no Docker, integration tests skipped)
	@echo "Running unit tests..."
	go test -short -race -timeout 120s ./...

test-integration: ## Run all tests including integration (requires Docker)
	@echo "Running all tests (unit + integration)..."
	AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test \
		go test -race -timeout 600s -v ./...

# ============================================================================
# Lint targets
# ============================================================================

lint: ## Lint all workspace modules
	@echo "Linting..."
	golangci-lint run ./...

lint-fix: ## Lint and auto-fix all workspace modules
	@echo "Linting with auto-fix..."
	golangci-lint run --fix ./...

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
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest
	go install github.com/icholy/gomajor@latest
	go install github.com/psampaz/go-mod-outdated@latest
	go install github.com/loov/goda@latest

check: build lint test ## Run full CI check (no Docker, integration skipped)

check-all: build lint test-integration ## Run full CI check including integration (Docker required)

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
