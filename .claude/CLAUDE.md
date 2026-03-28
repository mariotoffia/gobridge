# GoBridge - Claude Code Guidelines

This file provides guidance for Claude Code when working with the gobridge project.

## Project Overview

**GoBridge** is a message-bridge framework for Go. It routes messages between MQTT, AWS SQS, Azure Service Bus, HTTP, and other transports with pluggable processors, durable outbox delivery, dead-letter queue management, and observability.

### Key Characteristics

- **Multi-module Go workspace**: Core module has zero external dependencies; adapter dependencies are in separate modules
- **Go version**: 1.25+
- **Hexagonal architecture**: Domain types in `domain/`, port interfaces in `ports/`, implementations in `adapters/` and `processors/`
- **Pluggable architecture**: Transport, store, credential, and processor adapters via factory registration

## Project Structure

```
gobridge/
├── domain/                    # Pure value types (Envelope, RoutePolicy, errors) -- innermost ring
├── ports/                     # Port interfaces (Receiver, Sender, stores, Processor)
│   └── storetest/             # Conformance test suites for store implementations
├── runtime/                   # Route execution engine (Runtime, RouteRunner)
├── bridge/                    # Composition root (Builder wires config to runtime)
├── config/                    # Declarative YAML/JSON configuration model
├── validate/                  # Startup config validation
├── httpapi/                   # Admin and monitor HTTP servers
├── observability/             # Context helpers and correlation slog handler
├── adapters/
│   ├── mqtt/transport/paho/   # MQTT v5 via Paho (go.mod)
│   ├── aws/
│   │   ├── transport/sqs/     # AWS SQS (go.mod)
│   │   ├── store/             # DynamoDB store factory (go.mod per store)
│   │   ├── credentials/ssm/   # AWS SSM credentials (go.mod)
│   │   ├── metrics/cloudwatch/ # CloudWatch metrics (go.mod)
│   │   ├── config/dynamodb/   # DynamoDB config loader (go.mod)
│   │   └── cluster/ecs/       # ECS cluster resolver
│   ├── azure/transport/servicebus/ # Azure Service Bus (go.mod)
│   ├── http/transport/        # HTTP POST ingress, SSE egress (root module)
│   ├── native/
│   │   ├── store/             # Memory + SQLite stores (go.mod per store)
│   │   ├── credentials/file/  # File-based credentials (go.mod)
│   │   ├── config/file/       # File config loader (go.mod)
│   │   └── cluster/           # Native cluster resolver
│   └── otel/
│       ├── metrics/           # OTel OTLP metrics (go.mod)
│       └── tracing/           # OTel OTLP tracing (go.mod)
├── processors/                # ports.Processor implementations (go.mod each)
│   ├── filter/                # Condition-based filtering
│   ├── transform/             # JSON field mapping
│   ├── circuitbreaker/        # Circuit breaker
│   └── tenant/                # Multi-tenant validation
├── cmd/gobridge/              # Example binary
├── testutil/                  # Docker test helpers (DynamoDB, SQS, ASB, S3, MQTT, TLS)
└── tests/integration/         # End-to-end integration tests
```

## Build & Test Commands

```bash
# Build all modules
make build

# Run unit tests (with race detection, skips Docker-dependent tests)
make test
# Equivalent to: go test -short -race -timeout 120s ./...

# Run integration tests (requires Docker)
make test-integration

# Lint all modules
make lint

# Tidy dependencies
make tidy

# Update all dependencies
make update

# Full CI check locally
make check            # build + lint + unit tests (no Docker)
make check-all        # build + lint + all tests (Docker required)

# Docker test containers
make docker-up        # Start persistent test containers
make docker-down      # Stop containers
make docker-clean     # Remove ALL orphaned gobridge-* containers
```

## Coding Conventions

### Go Style

- Follow standard Go idioms and effective Go guidelines
- Use descriptive variable names; avoid single-letter names except for loop indices
- Error handling: Always check errors; wrap with context using `fmt.Errorf("context: %w", err)`
- Use structured errors with `domain.BridgeError` for transport operations

### Package Organization

- **Domain types in `domain/`**: Pure value types (Envelope, RoutePolicy, BridgeError, etc.)
- **Port interfaces in `ports/`**: Receiver, Sender, Session, stores, Processor, MetricsExporter, Tracer
- **Implementations in `adapters/` and `processors/`**: Each in a separate Go module
- **Option pattern**: Use functional options (`WithXxx(value)`) for configuration
- **Factory pattern**: Transports and stores use factory registration via `bridge.Builder`

### Interface Implementation

Verify interface satisfaction at compile time:

```go
var (
    _ ports.Receiver = (*Receiver)(nil)
    _ ports.Sender   = (*Sender)(nil)
)
```

### Error Handling

Use `domain.BridgeError` with `ErrorClass` to drive routing decisions:

```go
// Wrap infrastructure errors (transient / retriable)
return domain.ErrConnectionLost.Wrap(err)

// Wrap application errors (permanent / not retriable)
return domain.ErrInvalidPayload.With("topic", topic).Wrap(err)

// Classification
be, ok := domain.AsBridgeError(err)
recoverable := domain.IsRecoverableError(err)
```

Error classes: `Transient` (retriable), `Permanent` (not retriable), `Expired` (TTL exceeded), `Rejected` (payload-level).

### Logging

Use standard `*slog.Logger` with the `observability.CorrelationHandler` for contextual fields:

```go
logger.InfoContext(ctx, "operation completed",
    "id", id,
    "count", count,
)
```

The correlation handler automatically injects `correlation_id`, `trace_id`, and `span_id` from context.

### Flow Control

Routes implement backpressure via `RoutePolicy`:
- `MaxInFlight`: Limits concurrent messages per route (default: 100)
- `OnExpired` / `OnPermanentFailure`: Controls DLQ routing (`drop` or `dlq`)
- `DeliveryMode`: `direct_hold` (synchronous) or `shared_outbox` (durable async)

### Shared Connections

MQTT and similar stateful transports use `ports.Session` for shared connections:
- Multiple receivers/senders on one session
- `Session.Reconcile(ctx, SessionPlan)` converges subscriptions atomically

## Important Files to Know

| File | Purpose |
|------|---------|
| `ARCHITECTURE.md` | System design, hexagonal layers, core concepts, message flow |
| `DEVELOPMENT.md` | Prerequisites, workspace setup, building, testing, CI |
| `PLUGIN.md` | How to write transport, store, credential, and processor plugins |
| `TESTS.md` | Unit tests, conformance suites, integration tests, test utilities |
| `docs/configuration-overview.md` | Configuration lifecycle, sources, layered config |
| `docs/configuration-reference.md` | Field-by-field config reference |
| `docs/transport-configuration.md` | MQTT, SQS, Azure SB, HTTP transport options |
| `docs/processors-and-stores.md` | Processor chain and store backends |
| `docs/credentials-and-http-api.md` | Credential URI system and HTTP API endpoints |
| `docs/scenarios/` | 14 progressive scenario walkthroughs |

## Multi-Module Workflow

When working with transport modules:

```bash
# Work in specific module
cd adapters/mqtt/transport/paho
go build ./...
go test -v ./...

# Or use workspace from root
go build ./...  # Builds all modules via go.work
```

---

# SKILLS & AGENTS

This section lists available skills and agents with their filepaths to enable quick discovery and usage.

## Agents

**Location**: `.claude/agents/`

| Agent | Path | Purpose |
|-------|------|---------|
| ai-engineer | `.claude/agents/ai-engineer.md` | Expert AI engineer specializing in AI system design, model implementation, |
| api-designer | `.claude/agents/api-designer.md` | API architecture expert designing scalable, developer-friendly interfaces. |
| api-documenter | `.claude/agents/api-documenter.md` | Expert API documenter specializing in creating comprehensive, developer-friendly |
| architect-aws-serverless | `.claude/agents/architect-aws-serverless.md` | AWS serverless architecture expert for Lambda, API Gateway, DynamoDB, EventBridg |
| architect-gcp-serverless | `.claude/agents/architect-gcp-serverless.md` | GCP Serverless Architecture expert. Designs Cloud Run, Cloud Functions, Firestor |
| blockchain-developer | `.claude/agents/blockchain-developer.md` | Expert blockchain developer specializing in smart contract development, |
| build-engineer | `.claude/agents/build-engineer.md` | Expert build engineer specializing in build system optimization, compilation |
| cli-developer | `.claude/agents/cli-developer.md` | Expert CLI developer specializing in command-line interface design, developer |
| cloud-architect | `.claude/agents/cloud-architect.md` | Expert cloud architect specializing in multi-cloud strategies, scalable |
| code-reviewer | `.claude/agents/code-reviewer.md` | Expert code reviewer specializing in code quality, security vulnerabilities, |
| context-manager | `.claude/agents/context-manager.md` | Expert context manager specializing in information storage, retrieval, |
| cpp-pro | `.claude/agents/cpp-pro.md` | Expert C++ developer specializing in modern C++20/23, systems programming, |
| data-scientist | `.claude/agents/data-scientist.md` | Expert data scientist specializing in statistical analysis, machine learning, |
| database-optimizer | `.claude/agents/database-optimizer.md` | Expert database optimizer specializing in query optimization, performance |
| debugger | `.claude/agents/debugger.md` | Expert debugger specializing in complex issue diagnosis, root cause analysis, |
| deployment-engineer | `.claude/agents/deployment-engineer.md` | Expert deployment engineer specializing in CI/CD pipelines, release automation, |
| devops-engineer | `.claude/agents/devops-engineer.md` | Expert DevOps engineer bridging development and operations with comprehensive |
| devops-incident-responder | `.claude/agents/devops-incident-responder.md` | Expert incident responder specializing in rapid detection, diagnosis, |
| documentation-engineer | `.claude/agents/documentation-engineer.md` | Expert documentation engineer specializing in technical documentation |
| embedded-systems | `.claude/agents/embedded-systems.md` | Expert embedded systems engineer specializing in microcontroller programming, |
| error-detective | `.claude/agents/error-detective.md` | Expert error detective specializing in complex error pattern analysis, |
| frontend-developer | `.claude/agents/frontend-developer.md` | Expert UI engineer focused on crafting robust, scalable frontend solutions. |
| git-workflow-manager | `.claude/agents/git-workflow-manager.md` | Expert Git workflow manager specializing in branching strategies, automation, |
| golang-pro | `.claude/agents/golang-pro.md` | Expert Go developer specializing in high-performance systems, concurrent |
| graphql-architect | `.claude/agents/graphql-architect.md` | GraphQL schema architect designing efficient, scalable API graphs. Masters |
| incident-responder | `.claude/agents/incident-responder.md` | Expert incident responder specializing in security and operational incident |
| javascript-pro | `.claude/agents/javascript-pro.md` | Expert JavaScript developer specializing in modern ES2023+ features, |
| knowledge-synthesizer | `.claude/agents/knowledge-synthesizer.md` | Expert knowledge synthesizer specializing in extracting insights from |
| kubernetes-specialist | `.claude/agents/kubernetes-specialist.md` | Expert Kubernetes specialist mastering container orchestration, cluster |
| legacy-modernizer | `.claude/agents/legacy-modernizer.md` | Expert legacy system modernizer specializing in incremental migration |
| llm-architect | `.claude/agents/llm-architect.md` | Expert LLM architect specializing in large language model architecture, |
| mcp-developer | `.claude/agents/mcp-developer.md` | Expert MCP developer specializing in Model Context Protocol server and |
| ml-engineer | `.claude/agents/ml-engineer.md` | Expert ML engineer specializing in machine learning model lifecycle, |
| mlops-engineer | `.claude/agents/mlops-engineer.md` | Expert MLOps engineer specializing in ML infrastructure, platform engineering, |
| mobile-app-developer | `.claude/agents/mobile-app-developer.md` | Expert mobile app developer specializing in native and cross-platform |
| mobile-developer | `.claude/agents/mobile-developer.md` | Cross-platform mobile specialist building performant native experiences. |
| multi-agent-coordinator | `.claude/agents/multi-agent-coordinator.md` | Expert multi-agent coordinator specializing in complex workflow orchestration, |
| nextjs-developer | `.claude/agents/nextjs-developer.md` | Expert Next.js developer mastering Next.js 14+ with App Router and full-stack |
| penetration-tester | `.claude/agents/penetration-tester.md` | Expert penetration tester specializing in ethical hacking, vulnerability |
| performance-engineer | `.claude/agents/performance-engineer.md` | Expert performance engineer specializing in system optimization, bottleneck |
| platform-engineer | `.claude/agents/platform-engineer.md` | Expert platform engineer specializing in internal developer platforms, |
| prompt-engineer | `.claude/agents/prompt-engineer.md` | Expert prompt engineer specializing in designing, optimizing, and managing |
| python-pro | `.claude/agents/python-pro.md` | Expert Python developer specializing in modern Python 3.11+ development |
| qa-expert | `.claude/agents/qa-expert.md` | Expert QA engineer specializing in comprehensive quality assurance, test |
| react-specialist | `.claude/agents/react-specialist.md` | Expert React specialist mastering React 18+ with modern patterns and |
| refactoring-specialist | `.claude/agents/refactoring-specialist.md` | Expert refactoring specialist mastering safe code transformation techniques |
| rust-engineer | `.claude/agents/rust-engineer.md` | Expert Rust developer specializing in systems programming, memory safety, |
| sql-pro | `.claude/agents/sql-pro.md` | Expert SQL developer specializing in complex query optimization, database |
| technical-writer | `.claude/agents/technical-writer.md` | Expert technical writer specializing in clear, accurate documentation |
| terraform-engineer | `.claude/agents/terraform-engineer.md` | Expert Terraform engineer specializing in infrastructure as code, multi-cloud |
| test-automator | `.claude/agents/test-automator.md` | Expert test automation engineer specializing in building robust test |
| anti-patterns-auditor | `.claude/agents/anti-patterns-auditor.md` | Audits code and architecture for common anti-patterns. Identifies violations of  |
| bff-auditor | `.claude/agents/bff-auditor.md` | Audits BFF (Backend for Frontend) implementations for. Validates aggregat |
| clean-arch-reviewer | `.claude/agents/clean-arch-reviewer.md` | Reviews code for Clean Architecture compliance. Checks dependency direction, lay |
| code-interpreter | `.claude/agents/code-interpreter.md` | Transforms code into language-agnostic descriptions using JSON Schema, OpenAPI 3 |
| contract-reviewer | `.claude/agents/contract-reviewer.md` | Reviews API and event contracts for. Validates versioning, backward compa |
| cost-auditor | `.claude/agents/cost-auditor.md` | Cost optimization auditor for. Audits serverless infrastructure for cost  |
| ddd-expert | `.claude/agents/ddd-expert.md` | Domain-Driven Design expert for. Helps design aggregates, bounded |
| doc-reviewer | `.claude/agents/doc-reviewer.md` | Reviewer for documentation voice, structure, and style compliance |
| doc-writer | `.claude/agents/doc-writer.md` | Expert in writing AsciiDoc documentation with style and design system |
| event-architect | `.claude/agents/event-architect.md` | Expert in Event-Driven Architecture for. Designs domain events, |
| hexagonal-reviewer | `.claude/agents/hexagonal-reviewer.md` | Reviews code for Hexagonal Architecture (Ports & Adapters) compliance. |
| kpi-architect | `.claude/agents/kpi-architect.md` | KPI design expert for. Helps design business-aligned, actionable KPIs wit |
| microservice-auditor | `.claude/agents/microservice-auditor.md` | Audits microservice architecture for. Validates service boundaries, |
| observability-expert | `.claude/agents/observability-expert.md` | Observability expert for. Guides structured logging, metrics design, |
| resilience-auditor | `.claude/agents/resilience-auditor.md` | Resilience auditor for. Audits code and architecture for resilience |
| saga-expert | `.claude/agents/saga-expert.md` | Saga pattern expert for. Designs orchestrated and choreographed sagas wit |
| security-auditor | `.claude/agents/security-auditor.md` | Security auditor for. Audits code and architecture for authentication, |
| test-designer | `.claude/agents/test-designer.md` | Expert in designing test plans with proper classification and coverage |
| test-reviewer | `.claude/agents/test-reviewer.md` | Reviewer for test quality, determinism, and documentation compliance |
| typescript-pro | `.claude/agents/typescript-pro.md` | Expert TypeScript developer specializing in advanced type system usage, |
| ui-designer | `.claude/agents/ui-designer.md` | Expert visual designer specializing in creating intuitive, beautiful, |
| vue-expert | `.claude/agents/vue-expert.md` | Expert Vue specialist mastering Vue 3 with Composition API and ecosystem. |
| websocket-engineer | `.claude/agents/websocket-engineer.md` | Real-time communication specialist implementing scalable WebSocket architectures |
| workflow-orchestrator | `.claude/agents/workflow-orchestrator.md` | Expert workflow orchestrator specializing in complex process design, |

---

## Skills

**Location**: `.claude/skills/`

| Skill | Path | Purpose |
|-------|------|---------|
| create-bff-layer | `.claude/skills/create-bff-layer/SKILL.md` | Step-by-step guide for creating Backend for Frontend layers that aggregate multi |
| create-bounded-context | `.claude/skills/create-bounded-context/SKILL.md` | Step-by-step guide for creating DDD Bounded Contexts with ubiquitous language, c |
| create-microservice | `.claude/skills/create-microservice/SKILL.md` | Step-by-step guide for scaffolding a new microservice with Clean Architec |
| deployment-aws-canary-setup | `.claude/skills/deployment-aws-canary-setup/SKILL.md` | Step-by-step guide for setting up AWS Lambda canary deployments with CDK, CloudW |
| deployment-aws-serverless-setup | `.claude/skills/deployment-aws-serverless-setup/SKILL.md` | Step-by-step guide for setting up AWS serverless deployment with CDK, Lambda, an |
| deployment-gcp-canary-setup | `.claude/skills/deployment-gcp-canary-setup/SKILL.md` | Set up progressive canary deployments on GCP Cloud Run with traffic splitting, m |
| deployment-gcp-cloud-run-setup | `.claude/skills/deployment-gcp-cloud-run-setup/SKILL.md` | Step-by-step guide for setting up GCP Cloud Run infrastructure with Terraform, F |
| design-event-schema | `.claude/skills/design-event-schema/SKILL.md` | Step-by-step guide for designing domain event schemas with JSON Schema, versioni |
| implement-acl | `.claude/skills/implement-acl/SKILL.md` | Step-by-step guide for implementing Anti-Corruption Layers to shield your domain |
| implement-choreography | `.claude/skills/implement-choreography/SKILL.md` | Step-by-step guide for implementing choreography patterns with event bus, idempo |
| implement-circuit-breaker | `.claude/skills/implement-circuit-breaker/SKILL.md` | Step-by-step guide for implementing circuit breakers to protect against unstable |
| implement-cqrs-pattern | `.claude/skills/implement-cqrs-pattern/SKILL.md` | Step-by-step guide for implementing CQRS pattern with separate read/write models |
| implement-ddd-aggregate | `.claude/skills/implement-ddd-aggregate/SKILL.md` | Step-by-step guide for implementing DDD aggregates following patterns wit |
| implement-event-sourcing | `.claude/skills/implement-event-sourcing/SKILL.md` | Step-by-step guide for implementing event sourcing with event store, aggregate r |
| implement-hexagonal-ports | `.claude/skills/implement-hexagonal-ports/SKILL.md` | Step-by-step guide for implementing Hexagonal Architecture ports and adapters. |
| implement-saga-orchestration | `.claude/skills/implement-saga-orchestration/SKILL.md` | Step-by-step guide for implementing orchestrated sagas with state machines and c |
| implement-stateless-idempotency | `.claude/skills/implement-stateless-idempotency/SKILL.md` | Step-by-step guide for implementing stateless services and idempotent operations |
| implement-value-objects | `.claude/skills/implement-value-objects/SKILL.md` | Step-by-step guide for implementing DDD Value Objects with immutability and vali |
| setup-contract-tests | `.claude/skills/setup-contract-tests/SKILL.md` | Step-by-step guide for setting up contract tests with OpenAPI, JSON Schema, and  |
| setup-cost-optimization | `.claude/skills/setup-cost-optimization/SKILL.md` | Step-by-step guide for implementing cost optimization strategies for serverless  |
| setup-feature-flags | `.claude/skills/setup-feature-flags/SKILL.md` | Step-by-step guide for implementing feature flags to decouple deployment from re |
| setup-kpi-monitoring | `.claude/skills/setup-kpi-monitoring/SKILL.md` | Step-by-step guide for implementing business-aligned KPI monitoring with actiona |
| setup-observability | `.claude/skills/setup-observability/SKILL.md` | Step-by-step guide for implementing comprehensive observability with structured  |
| skill-asciidoc-documentation | `.claude/skills/skill-asciidoc-documentation/SKILL.md` | Write AsciiDoc documentation with style and design system |
| skill-create-test | `.claude/skills/skill-create-test/SKILL.md` | Design-first test creation workflow with validation |
