//go:build integration_aws || integration_local
// +build integration_aws integration_local

// Package integration hosts the opt-in AWS and local integration tests for the
// aws-filebased-config CDK profile.
//
// The `integration_aws` and `integration_local` build tags select the AWS and
// local suites respectively. Default `go build ./...` and `go test ./...`
// commands exclude both suites.
//
// Required AWS suite env vars (tests t.Skip when any is missing):
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
