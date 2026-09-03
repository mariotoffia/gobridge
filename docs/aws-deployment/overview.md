# AWS Deployment Overview

GoBridge runs on AWS as ECS Fargate services with EFS for configuration,
SSM Parameter Store for secrets, and an optional DynamoDB-coordinated HA
profile. This page covers the end-to-end architecture; the topologies, storage,
image, CDK constructs, and IAM policies each have their own page, listed under
[Page map](#page-map) below.

For generic deployment considerations, see [Deployment Guide](../deployment-guide.md).
For configuration details specific to AWS, see [Configuration on AWS](configuration.md).

---

## Architecture

The diagram below shows the full AWS architecture for a file-based GoBridge
deployment. Every component is created or referenced by the CDK constructs
described in [CDK Construct Library](cdk-constructs.md).

```mermaid
flowchart TD
    subgraph VPC["VPC (private subnets)"]
        subgraph ECS["ECS Fargate"]
            T1[Task 1\ngobridge-filebased]
            T2[Task N\ngobridge-filebased]
        end

        EFS[(EFS\nbridge.yaml)]
        ALB[Application\nLoad Balancer]
    end

    ECR[ECR\nContainer Registry] --> ECS
    SSM[SSM Parameter Store\nSecureString secrets] --> ECS
    CW[CloudWatch Logs\n& Metrics] --- ECS

    ALB --> T1
    ALB --> T2
    T1 -- NFS mount --> EFS
    T2 -- NFS mount --> EFS

    Client([External Clients]) --> ALB

    style EFS fill:#f5a623,stroke:#333,color:#000
    style SSM fill:#4a90d9,stroke:#333,color:#fff
    style ECR fill:#4a90d9,stroke:#333,color:#fff
    style CW fill:#4a90d9,stroke:#333,color:#fff
```

| Component | Role |
|-----------|------|
| **ECR** | Stores the `gobridge-filebased` container image. |
| **VPC** | Isolates the Fargate tasks in private subnets with NAT egress. |
| **ECS Fargate** | Runs the bridge container without EC2 instance management. |
| **EFS** | Provides a shared, hot-reloadable config file mount across all replicas. |
| **ALB** | Terminates TLS and routes HTTP traffic to the admin, monitor, or transport ports. |
| **SSM Parameter Store** | Holds API keys and credentials as `SecureString` parameters. |
| **CloudWatch** | Collects structured logs and optional custom metrics. |

---

---

## Page map

This overview covers the architecture and points at the rest. Each page below is
self-contained.

| Page | Covers |
|------|--------|
| [Deployment Topologies](topologies.md) | Single, cluster, and DynamoDB-coordinated HA; identity rules, task roles, alarms, failover proof. |
| [Compute and Runtime Metrics](compute.md) | Why ECS Fargate, task sizing, and the runtime metrics backend. |
| [Storage and Secrets](storage-and-secrets.md) | EFS for configuration, SSM Parameter Store for secrets, DynamoDB stores, DevMode guard. |
| [Container Image](container-image.md) | Production Dockerfile and the ECR lifecycle policy. |
| [CDK Construct Library](cdk-constructs.md) | Construct overview, props, and a complete usage example. |
| [IAM Least Privilege](iam.md) | Task-role and execution-role policies, statement by statement. |

---

## Related Guides

Beyond the pages in the map above.

| Guide | Description |
|-------|-------------|
| [Configuration on AWS](configuration.md) | Bridge YAML reference with AWS-specific settings. |
| [Monitoring and Observability](monitoring.md) | The CloudWatch exporter and the complete metric catalogue. |
| [CloudWatch alarms](alarms.md) | What the CDK bundle provisions, what `DefaultAlarms()` provisions, what nobody does, and the rollup metrics they all need. |
| [Logging, dashboards and tracing](logging-and-dashboards.md) | Structured logging, dashboard layout, ADOT/X-Ray tracing, log-metric filters, Grafana. |
| [HTTP API and Networking](http-api.md) | ALB target groups, security groups, and TLS termination. |
| [Total Cost of Ownership](tco.md) | Fargate, EFS, and SSM cost breakdown with worked examples. |
| [Running the Deployment Suite Locally](local-deployment-suite.md) | Deploying and driving this profile with no AWS account: what it proves, what it does not, and the measured emulation gaps. |
| [CDK Scenarios](../scenarios/cdk/) | Complete, runnable CDK deployment examples. |
| [Deployment Guide](../deployment-guide.md) | Platform-agnostic deployment considerations. |
