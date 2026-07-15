//go:build integration_aws
// +build integration_aws

// Package integration hosts the opt-in AWS integration tests for the
// aws-filebased-config CDK profile (T21 of
// 2026-05-07-aws-filebased-config-cdk-redesign-design.md).
//
// All files are guarded by the build tag `integration_aws`; the
// default `go build ./...` and `go test ./...` never see them.
//
// Required env vars (tests t.Skip when any is missing):
//
//	GOBRIDGE_INT_AWS_ACCOUNT   AWS account id used for the CDK env
//	GOBRIDGE_INT_AWS_REGION    AWS region used for the CDK env
//	GOBRIDGE_INT_VPC_ID        existing VPC id used for ECS + ALB
//	GOBRIDGE_INT_AVAILABILITY_ZONES comma-separated ordered AZs
//	GOBRIDGE_INT_SUBNET_IDS         comma-separated private subnet ids, one per AZ
//	GOBRIDGE_INT_PUBLIC_SUBNET_IDS  comma-separated public subnet ids, one per AZ
//
// Optional:
//
//	GOBRIDGE_INT_STACK_PREFIX  stack-name prefix (default "gobridge-it")
//	GOBRIDGE_INT_KEEP          "1" to skip teardown for post-mortem
//
// Tests assume the AWS CDK CLI (`cdk`) is on PATH and AWS credentials
// are resolvable by the standard provider chain.
package integration
