# Design: AWS Deployment Documentation & CDK Scenarios

## Context

The gobridge project has comprehensive documentation for bridge configuration (18 scenarios, reference docs) but lacks deployment guidance. The recent work in `deployment/aws-filebased-config/` created CDK constructs (L2 `GoBridgeEfsConfig`, `GoBridgeService`; L3 `GoBridgeStack`) and a zero-dependency `infra/` module for shared types. Developers adopting gobridge need documentation that bridges the gap between "I have a bridge config" and "I have a running, monitored, production-grade deployment on AWS."

## Goals

1. Generic deployment guide that works regardless of cloud provider
2. AWS-specific deep dives covering architecture, config lifecycle, monitoring, HTTP/API networking, and cost
3. Five progressive CDK scenarios from quickstart to multi-bridge cluster
4. Comprehensive (400-500 lines per file) with inline YAML, Go, and CDK code examples
5. Follow existing doc conventions: mermaid diagrams, field tables, prescriptive tone

## Non-Goals

- GCP or bare-metal deployment guides (future work)
- Changes to the CDK constructs themselves (already implemented)
- Changes to the runtime bootstrap code

---

## File Structure

```
docs/
  deployment-guide.md                       # Generic, cloud-agnostic
  aws-deployment/
    overview.md                             # AWS architecture & design
    configuration.md                        # Config lifecycle on AWS
    monitoring.md                           # CloudWatch, alarms, tracing
    http-api.md                             # Ports, ALB, API Gateway
    tco.md                                  # Total cost of ownership
  scenarios/
    cdk/
      01-quickstart-default-vpc.md          # Batteries-included L3 stack
      02-custom-vpc.md                      # Bring-your-own VPC/ALB/cluster
      03-api-gateway.md                     # HTTP transport behind APIGW
      04-production-stack.md                # Full monitoring & hardening
      05-multi-bridge-cluster.md            # filesystem_replicated topology
```

### Link Topology

- `deployment-guide.md` links to `aws-deployment/overview.md` as the AWS entry point
- Each `aws-deployment/*.md` links to relevant CDK scenarios for runnable examples
- CDK scenarios link back to `aws-deployment/*.md` for concept explanation
- CDK scenarios cross-link progressively (01 → 02 → 04, 01 → 03, 04 → 05)
- All files link back to existing docs (`configuration-overview.md`, `credentials-and-http-api.md`, etc.) where applicable

---

## File Specifications

### `docs/deployment-guide.md` (~400-450 lines)

Generic, cloud-agnostic deployment considerations.

**Sections:**

| Section | Content |
|---------|---------|
| Deployment Models | standalone vs clustered, single vs filesystem_replicated, decision table |
| Configuration Delivery | mounted file, env var, remote store; poll vs notify trade-offs |
| Secret Management | `pms://` credential URI, admin/monitor API keys, secret flow diagram |
| Networking & Ports | three HTTP servers (:8080, :8081, :8082), mermaid port architecture diagram, internal vs exposed guidance |
| Health Checks & Graceful Shutdown | `/healthz`, drain/shutdown timeouts, signal handling, orchestrator integration |
| Observability | structured logging, correlation IDs, metrics export, tracing, alert recommendations |
| Scaling Considerations | MaxInFlight, CPU/memory sizing, horizontal vs vertical scaling |
| What's Next | links to `aws-deployment/overview.md` |

**Includes:** 1 mermaid diagram (three-port architecture), 2 YAML snippets (bootstrap config, bridge config excerpt), 1 decision table (topology selection).

---

### `docs/aws-deployment/overview.md` (~450 lines)

AWS architecture and design decisions.

**Sections:**

| Section | Content |
|---------|---------|
| Architecture Diagram | mermaid: ECR → Fargate → EFS + SSM + CloudWatch, ALB in front |
| Why ECS Fargate | serverless containers, sizing guidance, Fargate Spot |
| EFS for Configuration | why EFS over S3/env vars, access point design, POSIX permissions |
| SSM Parameter Store for Secrets | SecureString, DevMode guard, KMS, credential URI mapping |
| Container Image | multi-stage Dockerfile example, ECR lifecycle policies |
| CDK Construct Library | L2/L3 overview, import paths, props summary table |
| IAM Least Privilege | task role (SSM, EFS, SQS), execution role (ECR, Logs), scoped ARNs |

**Includes:** 1 architecture diagram, 1 Dockerfile, 1 IAM policy JSON, props summary table.

---

### `docs/aws-deployment/configuration.md` (~450 lines)

Config lifecycle on AWS.

**Sections:**

| Section | Content |
|---------|---------|
| Two Config Layers | bootstrap (env var) vs bridge (YAML on EFS), what goes where |
| Bootstrap Config Reference | full field table with types, defaults, descriptions |
| Bridge Config on EFS | writing config (init container, CI/CD, manual), path conventions |
| Hot-Reload | poll watcher on EFS, why notify doesn't work on NFS, poll interval tuning |
| Config Updates in Production | step-by-step reload flow, invalid config handling (keeps last good) |
| Environment Variable Injection | `GOBRIDGE_FILEBASED_BOOTSTRAP_JSON`, CDK auto-injection |
| Topology Modes | single vs filesystem_replicated, comparison table |

**Includes:** 1 sequence diagram (hot-reload flow), bootstrap config field table, 3 YAML examples, topology comparison table.

---

### `docs/aws-deployment/monitoring.md` (~450 lines)

Observability on AWS.

**Sections:**

| Section | Content |
|---------|---------|
| Structured Logging | JSON via slog, correlation handler, CloudWatch Logs Insights queries |
| CloudWatch Metrics | namespace/dimensions, key metrics table, custom metric example |
| CloudWatch Alarms | recommended alarms table with thresholds, SNS wiring |
| CloudWatch Dashboard | JSON dashboard definition, widget descriptions |
| Distributed Tracing | X-Ray via OTLP adapter, trace propagation, sampling config |
| Log-Based Metric Filters | patterns for circuit breaker, config reload, errors |
| Connecting to Grafana | CloudWatch as Grafana data source |

**Includes:** 1 observability data flow diagram, Logs Insights query examples, alarm definition table, dashboard JSON snippet.

---

### `docs/aws-deployment/http-api.md` (~450 lines)

HTTP API and networking on AWS.

**Sections:**

| Section | Content |
|---------|---------|
| Port Architecture | three servers in AWS context, target group mapping |
| ALB Configuration | internal for admin/monitor, optional internet-facing, health checks |
| API Gateway Integration | REST vs HTTP API, VPC Link, usage plans, API keys |
| Authentication Patterns | X-API-Key (SSM-sourced), APIGW authorizers, mutual TLS |
| CORS Configuration | `cors_origins` field, APIGW CORS, preflight |
| SSE Egress | ALB timeout considerations, WebSocket API alternative |
| Network Topology | mermaid: Internet → APIGW → ALB → Fargate → EFS/SSM/SQS |

**Includes:** 1 network topology diagram, API Gateway OpenAPI snippet, ALB listener rule CDK code, authentication flow diagram.

---

### `docs/aws-deployment/tco.md` (~400 lines)

Total cost of ownership.

**Sections:**

| Section | Content |
|---------|---------|
| Cost Model Overview | mermaid diagram of cost components |
| Fargate Compute | vCPU/memory pricing, Fargate Spot savings, sizing table |
| EFS Costs | storage classes, lifecycle policies, negligible for config |
| Networking | NAT Gateway (hidden cost), VPC endpoints comparison table |
| Observability Costs | Logs ingestion/storage, metric pricing, alarm pricing, X-Ray sampling |
| API Gateway vs ALB | cost comparison at 1K/100K/1M requests/day |
| Reference Architectures | three profiles with monthly estimates: Dev (~$15-25), Production Single (~$80-120), Production Cluster (~$200-350) |
| Cost Optimization Checklist | actionable items: Spot, VPC endpoints, log retention, metric filters |

**Includes:** cost component diagram, 3 pricing comparison tables, reference architecture cost breakdown, optimization checklist.

---

### `docs/scenarios/cdk/01-quickstart-default-vpc.md` (~350 lines)

**Title:** CDK Scenario 1: Quickstart with Default VPC

**Story:** Deploy gobridge on AWS in 10 minutes with zero existing infrastructure.

| Section | Content |
|---------|---------|
| Use Case | evaluating gobridge, want a running instance fast |
| Architecture | mermaid: New VPC → Fargate → EFS, single task |
| Prerequisites | AWS account, CDK CLI, Go 1.25+, Docker |
| Build & Push Image | multi-stage Dockerfile, ECR push |
| CDK Stack | complete Go code using L3 `NewGoBridgeStack`, env vars, `cdk deploy` |
| Write Bridge Config | minimal YAML, write to EFS via CLI |
| Verify | curl admin API, check logs, send test message |
| Clean Up | `cdk destroy`, retention warnings |
| What's Next | links to 02 and 04 |

---

### `docs/scenarios/cdk/02-custom-vpc.md` (~400 lines)

**Title:** CDK Scenario 2: Custom VPC & Existing Infrastructure

**Story:** Add gobridge to an established AWS environment with existing VPC, ALB, and ECS cluster.

| Section | Content |
|---------|---------|
| Use Case | enterprise landing zone, shared infra |
| Architecture | mermaid: existing VPC → existing cluster → GoBridgeService → existing ALB |
| VPC Lookup | `Vpc_FromLookup`, subnet selection |
| Cluster Import | `Cluster_FromClusterAttributes` |
| ALB Integration | existing ALB, listener rules, path vs port routing |
| Security Groups | task → EFS, task → ALB, VPC CIDR restriction |
| Complete CDK Code | full L2 usage with imported resources |
| EFS in Shared VPC | cross-AZ mounts, existing EFS reuse |
| Variations | shared EFS across services, cross-account, PrivateLink |

---

### `docs/scenarios/cdk/03-api-gateway.md` (~450 lines)

**Title:** CDK Scenario 3: HTTP Transport Behind API Gateway

**Story:** Expose gobridge HTTP transport to external clients with managed auth, throttling, and custom domain.

| Section | Content |
|---------|---------|
| Use Case | SaaS platform, external partner integration |
| Architecture | mermaid: Internet → APIGW → VPC Link → NLB → Fargate :8082 |
| Why API Gateway | managed TLS, usage plans, WAF, cost at low volume |
| VPC Link & NLB | why NLB for VPC Link, CDK code |
| REST API Definition | OpenAPI snippet, method config |
| Usage Plans & API Keys | CDK code, throttle/quota |
| Custom Domain | Route53 + ACM + domain mapping |
| Complete CDK Code | full stack combining GoBridgeService + APIGW |
| Testing | curl with API key, CloudWatch metrics |
| Variations | HTTP API, Lambda authorizer, mTLS, WebSocket for SSE |

---

### `docs/scenarios/cdk/04-production-stack.md` (~500 lines)

**Title:** CDK Scenario 4: Production-Ready Stack with Monitoring

**Story:** Go live with alarms, dashboards, auto-scaling, hardened security, and operational visibility.

| Section | Content |
|---------|---------|
| Use Case | business-critical messages, SLA, on-call team |
| Architecture | comprehensive mermaid: multi-AZ Fargate, CloudWatch, SNS, X-Ray |
| Security Hardening | non-root, read-only FS, CMK, scoped IAM, VPC endpoints |
| Auto-Scaling | CPU tracking, SQS depth, scheduled, CDK code |
| CloudWatch Alarms | CDK for all recommended alarms |
| CloudWatch Dashboard | CDK dashboard construct with widgets |
| SSM Secrets Management | CDK SecureString, rotation strategy |
| Structured Logging | log group, Insights queries, metric filters |
| X-Ray Tracing | OTLP adapter config, daemon sidecar, sampling |
| Config Management Pipeline | CI/CD → EFS via DataSync |
| Complete CDK Code | full production assembly |
| Cost Notes | links to tco.md |

---

### `docs/scenarios/cdk/05-multi-bridge-cluster.md` (~450 lines)

**Title:** CDK Scenario 5: Multi-Bridge Cluster with Shared EFS

**Story:** Multiple bridge instances with control/worker topology sharing configuration.

| Section | Content |
|---------|---------|
| Use Case | high-throughput, control manages config, workers process |
| Architecture | mermaid: EFS ← Control + Workers |
| Topology: filesystem_replicated | supported/unsupported features |
| Control vs Worker | NodeRole differences, bootstrap config |
| Two CDK Services | separate GoBridgeService for control (1) and workers (N) |
| Service Discovery | Cloud Map, cluster endpoints config |
| EFS Write Access | control RW, workers RO, access points |
| Config Propagation | write → detect → rebuild sequence, timing |
| Complete CDK Code | full cluster with control + workers + shared EFS |
| Scaling Workers | independent auto-scaling, SQS backlog |
| Variations | mixed transports, blue-green config, canary rollout |

---

## Conventions

All files follow existing gobridge documentation patterns:

- **Mermaid diagrams** for architecture and data flow (flowchart LR/TD, sequenceDiagram)
- **Markdown tables** for field references, comparisons, decision matrices
- **Fenced code blocks** with language tags: ` ```yaml `, ` ```go `, ` ```json `, ` ```bash `
- **Prescriptive tone**: "you should", "we recommend", with decision guidance
- **Cross-links** using relative paths: `[Overview](overview.md)`, `[Scenario 4](../scenarios/cdk/04-production-stack.md)`
- **H1 title**, H2 for major sections, H3 for subsections
- **CDK scenarios** follow: Use Case → Architecture → Config/Code → Verify → Variations

## Verification

- All internal links resolve (no broken cross-references)
- All YAML examples are valid gobridge config (parseable by `config.ParseFile`)
- All Go/CDK code compiles (verified against the existing `cdk/constructs/` package)
- All mermaid diagrams render (no syntax errors)
- No file exceeds 600 lines (docs limit from CLAUDE.md)
