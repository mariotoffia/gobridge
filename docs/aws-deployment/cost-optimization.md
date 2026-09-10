# Cost optimization checklist

Use this checklist to reduce your monthly bill without sacrificing reliability.

### Compute

- [ ] **Use Fargate Spot for non-critical workloads.** Spot provides 70%
  savings. Use it for development, staging, and worker tasks that tolerate
  interruption.
- [ ] **Right-size Fargate tasks.** Monitor CPU and memory utilization via
  CloudWatch. If utilization stays below 30%, step down to a smaller
  configuration.
- [ ] **Consider Fargate Savings Plans.** A 1-year Compute Savings Plan
  reduces on-demand costs by up to 50% for predictable baseline workloads.
- [ ] **Enable auto-scaling.** Scale down during off-peak hours rather than
  running peak capacity 24/7.

### Networking

- [ ] **Replace NAT Gateway with VPC endpoints.** If you only need access to
  SSM, SQS, ECR, and CloudWatch Logs, two or three VPC endpoints cost less
  than a NAT Gateway.
- [ ] **Use a single AZ for development.** VPC endpoint costs double with two
  AZs. Use one AZ for non-production environments.
- [ ] **Deploy in a public subnet for dev/test.** Eliminates NAT and VPC
  endpoint costs entirely.

### Observability

- [ ] **Set log retention to the minimum needed.** 7 days for dev, 30 days
  for staging, 90 days for production.
- [ ] **Use metric filters instead of custom metrics.** CloudWatch metric
  filters extract metrics from logs at no additional cost beyond ingestion.
- [ ] **Sample X-Ray traces.** Use 1--10% sampling in production. Full
  sampling is rarely necessary and costs $5 per million traces.
- [ ] **Export old logs to S3.** For retention beyond 90 days, export to S3
  at $0.023/GB-month instead of keeping them in CloudWatch at $0.03/GB-month.

### API Layer

- [ ] **Use HTTP API instead of REST API.** HTTP API costs $1/M requests
  versus $3.50/M for REST API. Switch to REST only when you need usage plans
  or request validation.
- [ ] **Skip ALB for low-traffic deployments.** At fewer than 500K requests
  per day, HTTP API Gateway is significantly cheaper than the ALB fixed cost.

### Storage

- [ ] **Review EFS throughput mode.** Elastic throughput is the best default.
  Never provision throughput for a config-only mount.

---
