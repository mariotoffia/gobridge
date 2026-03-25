# Makefile for gobridge multi-module workspace
#
# This Makefile provides convenient commands for building, testing, and
# maintaining the multi-module Go workspace.

.PHONY: all build test test-integration lint lint-fix clean tidy sync help
.PHONY: build-core build-mqtt build-aws build-azure

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

tidy: ## Tidy all module dependencies (discovers all go.mod files recursively)
	@find . -name go.mod -not -path '*/vendor/*' -execdir sh -c 'echo "Tidying $$(pwd)..." && go mod tidy' \;

sync: ## Sync workspace and update dependencies
	@echo "Syncing workspace..."
	go work sync
	@$(MAKE) tidy

update: ## Update all dependencies to latest versions
	@echo "Updating core dependencies..."
	go get -u ./...
	go mod tidy
	@echo "Updating MQTT dependencies..."
	cd transport/mqtt && go get -u ./... && go mod tidy
	@echo "Updating AWS dependencies..."
	cd transport/aws && go get -u ./... && go mod tidy
	@echo "Updating Azure dependencies..."
	cd transport/azure && go get -u ./... && go mod tidy
	@$(MAKE) sync

clean: ## Clean build cache and test cache
	@echo "Cleaning build cache..."
	go clean -cache -testcache ./...
	cd transport/mqtt && go clean -cache -testcache ./...
	cd transport/aws && go clean -cache -testcache ./...
	cd transport/azure && go clean -cache -testcache ./...

# ============================================================================
# Development helpers
# ============================================================================

dev-deps: ## Install development dependencies
	@echo "Installing development dependencies..."
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

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
