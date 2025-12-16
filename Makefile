# Makefile for gobridge multi-module workspace
#
# This Makefile provides convenient commands for building, testing, and
# maintaining the multi-module Go workspace.

.PHONY: all build test lint clean tidy sync help
.PHONY: build-core build-mqtt build-aws build-azure
.PHONY: lint-core lint-mqtt lint-aws lint-azure

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

test: ## Test core module
	go test -v -race ./...

# ============================================================================
# Lint targets
# ============================================================================

lint: lint-core lint-mqtt lint-aws lint-azure ## Lint all modules

lint-core: ## Lint core module
	@echo "Linting core module..."
	golangci-lint run ./...

lint-mqtt: ## Lint MQTT module
	@echo "Linting MQTT module..."
	cd transport/mqtt && golangci-lint run ./...

lint-aws: ## Lint AWS module
	@echo "Linting AWS module..."
	cd transport/aws && golangci-lint run ./...

lint-azure: ## Lint Azure module
	@echo "Linting Azure module..."
	cd transport/azure && golangci-lint run ./...

# ============================================================================
# Maintenance targets
# ============================================================================

tidy: ## Tidy all module dependencies
	@echo "Tidying core module..."
	go mod tidy
	@echo "Tidying MQTT module..."
	cd transport/mqtt && go mod tidy
	@echo "Tidying AWS module..."
	cd transport/aws && go mod tidy
	@echo "Tidying Azure module..."
	cd transport/azure && go mod tidy

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

check: build lint test ## Run full CI check locally

# ============================================================================
# Docker test containers
# ============================================================================

docker-up: ## Start test containers (Mosquitto, LocalStack)
	@echo "Starting test containers..."
	docker run -d --name gobridge-mosquitto -p 1883:1883 eclipse-mosquitto:latest
	docker run -d --name gobridge-localstack -p 4566:4566 localstack/localstack:latest

docker-down: ## Stop and remove test containers
	@echo "Stopping test containers..."
	-docker rm -f gobridge-mosquitto gobridge-localstack

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
