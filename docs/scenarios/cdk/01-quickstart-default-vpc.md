# CDK Scenario 1: Quickstart with Default VPC

Deploy GoBridge on AWS ECS Fargate in under 10 minutes with zero existing infrastructure.

## Use Case

You are a developer evaluating GoBridge and want a running instance as quickly as possible. You
have an AWS account but no existing VPC, ECS cluster, or EFS filesystem. The L3
`NewGoBridgeStack` construct creates everything for you -- VPC, EFS, ECS cluster, and a single
Fargate task -- so you can focus on bridge configuration instead of infrastructure plumbing.

## Architecture

```mermaid
flowchart LR
    subgraph AWS Account
        ALB["ALB\n(admin + monitor)"]

        subgraph VPC ["New VPC (2 AZs)"]
            subgraph Fargate ["ECS Fargate"]
                Task["gobridge task\n512 CPU / 1024 MiB"]
            end
            EFS["EFS\n/gobridge/bridge.yaml"]
        end
    end

    Client["Developer\ncurl / browser"] -->|HTTP| ALB
    ALB --> Task
    Task -->|NFS mount\n/mnt/gobridge| EFS

    style Task fill:#f96,stroke:#333
    style EFS fill:#6bf,stroke:#333
```

The stack provisions:

- A new VPC with public and private subnets across 2 availability zones.
- An encrypted EFS filesystem with an access point at `/gobridge`.
- A single Fargate task running the gobridge container image.
- Port mappings for the admin API (8080) and monitor API (8081).
- Auto-scaling from 1 to 4 tasks based on CPU utilization (70% target).

## Prerequisites

| Requirement       | Minimum version | Check command            |
|-------------------|-----------------|--------------------------|
| AWS account       | --              | `aws sts get-caller-identity` |
| AWS CLI           | 2.x             | `aws --version`          |
| AWS CDK CLI       | 2.x             | `cdk --version`          |
| Go                | 1.25+           | `go version`             |
| Docker            | 20.x+           | `docker --version`       |
| CDK bootstrapped  | --              | `cdk bootstrap aws://ACCOUNT/REGION` |

Ensure your shell has valid AWS credentials:

```bash
export CDK_DEFAULT_ACCOUNT=$(aws sts get-caller-identity --query Account --output text)
export CDK_DEFAULT_REGION=us-west-1
```

## Build and Push Container Image

GoBridge ships as a Go binary. Use a multi-stage Dockerfile to produce a minimal container.

Create `Dockerfile` in the repository root:

```dockerfile
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.work go.work.sum ./
COPY . .
RUN go build -o /gobridge ./cmd/gobridge

FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget
COPY --from=build /gobridge /usr/local/bin/gobridge
ENTRYPOINT ["gobridge"]
CMD ["--config", "/mnt/gobridge/bridge.yaml"]
```

Create an ECR repository, build the image, and push it:

```bash
ACCOUNT=$(aws sts get-caller-identity --query Account --output text)
REGION=us-west-1
REPO_URI="${ACCOUNT}.dkr.ecr.${REGION}.amazonaws.com/gobridge"

# Create ECR repository (skip if it already exists)
aws ecr create-repository \
  --repository-name gobridge \
  --region "${REGION}" || true

# Authenticate Docker with ECR
aws ecr get-login-password --region "${REGION}" | \
  docker login --username AWS --password-stdin "${ACCOUNT}.dkr.ecr.${REGION}.amazonaws.com"

# Build and push
docker build -t gobridge:latest -f Dockerfile .
docker tag gobridge:latest "${REPO_URI}:latest"
docker push "${REPO_URI}:latest"
```

## Create SSM Parameter

The admin API requires an API key stored in AWS Systems Manager Parameter Store. The Fargate
task reads it at startup via the `AdminAPIKeyParam` bootstrap field.

```bash
aws ssm put-parameter \
  --name /gobridge/admin-api-key \
  --type SecureString \
  --value "my-secret-admin-key-min16chars" \
  --region us-west-1
```

The value must be at least 16 characters. Choose a strong, random string for production use.

## CDK Stack

The CDK application lives in `deployment/aws-filebased-config/cdk/`. The provided `main.go`
reads all configuration from environment variables, so you do not need to edit Go source code.

### Environment variables

| Variable                   | Default                       | Description                        |
|----------------------------|-------------------------------|------------------------------------|
| `GOBRIDGE_STACK_NAME`      | `GoBridge`                    | CloudFormation stack name          |
| `GOBRIDGE_SERVICE_NAME`    | `gobridge`                    | ECS service name                   |
| `GOBRIDGE_IMAGE_URI`       | `gobridge:latest`             | Container image URI (ECR)          |
| `GOBRIDGE_BRIDGE_ID`       | `gobridge-main`               | Bridge instance identifier         |
| `GOBRIDGE_CONFIG_PATH`     | `/mnt/gobridge/bridge.yaml`   | Config file path inside container  |
| `GOBRIDGE_ADMIN_KEY_PARAM` | `/gobridge/admin-api-key`     | SSM parameter name for admin key   |
| `GOBRIDGE_VPC_ID`          | *(empty -- creates new VPC)*  | Existing VPC ID to reuse           |

### Deploy

```bash
cd deployment/aws-filebased-config/cdk

ACCOUNT=$(aws sts get-caller-identity --query Account --output text)
REGION=us-west-1

export GOBRIDGE_IMAGE_URI="${ACCOUNT}.dkr.ecr.${REGION}.amazonaws.com/gobridge:latest"
export GOBRIDGE_ADMIN_KEY_PARAM="/gobridge/admin-api-key"

cdk deploy --require-approval broadening
```

Because `GOBRIDGE_VPC_ID` is unset, the stack calls `awsec2.NewVpc` with `MaxAzs: 2` to create
a brand-new VPC. The `GoBridgeStackProps` struct drives this behaviour:

```go
type GoBridgeStackProps struct {
    awscdk.StackProps

    ServiceName string
    ImageURI    string
    Bootstrap   infra.BootstrapConfig
    Exposure    infra.Exposure

    // VpcID is an existing VPC to look up. If empty, a new VPC is created.
    VpcID  string
    MaxAZs *float64
}
```

The bootstrap configuration passed to the task container as the
`GOBRIDGE_FILEBASED_BOOTSTRAP_JSON` environment variable looks like this after defaults are
applied:

```json
{
  "bridge_id": "gobridge-main",
  "node_role": "control",
  "topology": "single",
  "config_file_path": "/mnt/gobridge/bridge.yaml",
  "admin_addr": ":8080",
  "monitor_addr": ":8081",
  "transport_http_addr": ":8082",
  "admin_api_key_param": "/gobridge/admin-api-key"
}
```

## Write Bridge Config to EFS

The Fargate task expects a bridge configuration file at `/mnt/gobridge/bridge.yaml` on the
EFS mount. Create a minimal config that accepts HTTP requests and forwards them to an external
HTTP endpoint for testing.

### Bridge configuration

Save the following as `bridge.yaml` locally:

```yaml
bridge:
  id: quickstart
  log_level: info

receivers:
  - id: http-in
    transport: http
    options:
      path: /ingest

senders:
  - id: http-out
    transport: http
    options:
      url: https://httpbin.org/post

bindings:
  - id: to-httpbin
    sender_id: http-out

routes:
  - id: forward
    receiver_id: http-in
    bindings: [to-httpbin]
```

This config creates a single route: HTTP POST requests to `/ingest` on the transport HTTP port
(8082) are forwarded to `https://httpbin.org/post`.

### Upload to EFS

EFS is only accessible from within the VPC. Use a one-shot ECS task to copy the file. The
following script creates a temporary task definition, runs it, and cleans up:

```bash
# Resolve stack outputs
CLUSTER=$(aws ecs list-clusters --query "clusterArns[?contains(@, 'GoBridge')]|[0]" --output text)
SUBNETS=$(aws ec2 describe-subnets \
  --filters "Name=tag:aws-cdk:subnet-name,Values=Private" \
  --query "Subnets[*].SubnetId" --output text | tr '\t' ',')
SG=$(aws ec2 describe-security-groups \
  --filters "Name=description,Values=gobridge EFS mount target access" \
  --query "SecurityGroups[0].GroupId" --output text)
FS_ID=$(aws efs describe-file-systems \
  --query "FileSystems[?Name!=null]|[0].FileSystemId" --output text)
AP_ID=$(aws efs describe-access-points \
  --file-system-id "${FS_ID}" --query "AccessPoints[0].AccessPointId" --output text)

# Encode config as base64
CONFIG_B64=$(base64 < bridge.yaml)

# Register a one-shot task definition
cat > /tmp/efs-writer-task.json <<EOF
{
  "family": "gobridge-efs-writer",
  "networkMode": "awsvpc",
  "requiresCompatibilities": ["FARGATE"],
  "cpu": "256",
  "memory": "512",
  "volumes": [{
    "name": "gobridge-config",
    "efsVolumeConfiguration": {
      "fileSystemId": "${FS_ID}",
      "transitEncryption": "ENABLED",
      "authorizationConfig": {
        "accessPointId": "${AP_ID}",
        "iam": "ENABLED"
      }
    }
  }],
  "containerDefinitions": [{
    "name": "writer",
    "image": "alpine:3.20",
    "essential": true,
    "command": ["sh", "-c", "echo '${CONFIG_B64}' | base64 -d > /mnt/gobridge/bridge.yaml && echo 'Config written.' && sleep 2"],
    "mountPoints": [{
      "sourceVolume": "gobridge-config",
      "containerPath": "/mnt/gobridge"
    }]
  }]
}
EOF

aws ecs register-task-definition --cli-input-json file:///tmp/efs-writer-task.json
aws ecs run-task \
  --cluster "${CLUSTER}" \
  --task-definition gobridge-efs-writer \
  --launch-type FARGATE \
  --network-configuration "awsvpcConfiguration={subnets=[${SUBNETS}],securityGroups=[${SG}]}" \
  --count 1
```

Wait for the task to reach STOPPED status, then the gobridge service will pick up the config
file on its next poll (default: every 1 second).

## Verify

After the stack deploys and the config file is on EFS, verify that GoBridge is running.

### Health check

The admin API exposes a health endpoint:

```bash
ALB_DNS=$(aws elbv2 describe-load-balancers \
  --query "LoadBalancers[?contains(DNSName, 'gobridge') || contains(LoadBalancerName, 'GoBridge')]|[0].DNSName" \
  --output text 2>/dev/null)

# If no ALB, use ECS task public IP (when running with public subnets)
if [ -z "${ALB_DNS}" ] || [ "${ALB_DNS}" = "None" ]; then
  TASK_ARN=$(aws ecs list-tasks --cluster "${CLUSTER}" --service-name gobridge --query "taskArns[0]" --output text)
  ENI=$(aws ecs describe-tasks --cluster "${CLUSTER}" --tasks "${TASK_ARN}" \
    --query "tasks[0].attachments[0].details[?name=='networkInterfaceId'].value" --output text)
  TASK_IP=$(aws ec2 describe-network-interfaces --network-interface-ids "${ENI}" \
    --query "NetworkInterfaces[0].Association.PublicIp" --output text)
  ENDPOINT="http://${TASK_IP}:8080"
else
  ENDPOINT="http://${ALB_DNS}"
fi

curl -s "${ENDPOINT}/healthz"
```

Expected response:

```json
{"status":"healthy"}
```

### Admin config API

Retrieve the running configuration:

```bash
curl -s -H "X-API-Key: my-secret-admin-key-min16chars" \
  "${ENDPOINT}/admin/v1/config" | jq .
```

### Send a test message

Post a message to the HTTP transport port (8082) and verify it reaches httpbin:

```bash
curl -s -X POST \
  -H "Content-Type: application/json" \
  -d '{"sensor":"temp-1","value":23.5}' \
  "http://${TASK_IP}:8082/ingest"
```

A successful response means the message was forwarded to `https://httpbin.org/post`.

## Clean Up

Remove all provisioned resources:

```bash
cd deployment/aws-filebased-config/cdk
cdk destroy --force
```

**EFS retention warning** -- The EFS filesystem uses the default `RETAIN` removal policy. After
`cdk destroy`, the filesystem and its data persist in your account. Delete it manually if you
no longer need it:

```bash
aws efs delete-file-system --file-system-id "${FS_ID}"
```

Also clean up the ECR repository and SSM parameter:

```bash
aws ecr delete-repository --repository-name gobridge --force --region us-west-1
aws ssm delete-parameter --name /gobridge/admin-api-key --region us-west-1
```

## What's Next

- [CDK Scenario 2: Custom VPC](02-custom-vpc.md) -- deploy into an existing VPC with private
  subnets and a NAT gateway.
- [CDK Scenario 4: Production Stack](04-production-stack.md) -- add monitoring, alerting, and
  security hardening for production workloads.
- [AWS Deployment Overview](../../aws-deployment/overview.md) -- understand the full
  architecture, deployment profiles, and operational model.
- [Scenario 1: MQTT-to-MQTT](../01-mqtt-to-mqtt.md) -- explore bridge configuration patterns
  starting with the simplest MQTT forwarding setup.
