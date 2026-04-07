// Package main provides a CDK application for deploying the gobridge
// file-based configuration profile on AWS ECS Fargate.
package main

import (
	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"

	gobridgecdk "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

// GoBridgeStackProps configures the opinionated L3 stack that deploys
// gobridge with a VPC, EFS, and Fargate service in one shot.
type GoBridgeStackProps struct {
	awscdk.StackProps

	// ServiceName is the ECS service name.
	ServiceName string

	// ImageURI is the container image URI (e.g. ECR repo:tag).
	ImageURI string

	// Bootstrap is the gobridge bootstrap configuration.
	Bootstrap infra.BootstrapConfig

	// Exposure controls which ports are exposed.
	Exposure infra.Exposure

	// VpcID is an existing VPC to look up. If empty, a new VPC is created.
	VpcID string

	// MaxAZs is the max availability zones for the VPC. Default: 2.
	MaxAZs *float64
}

// NewGoBridgeStack creates a complete, opinionated stack for gobridge.
func NewGoBridgeStack(scope constructs.Construct, id string, props *GoBridgeStackProps) awscdk.Stack {
	stack := awscdk.NewStack(scope, &id, &props.StackProps)

	var vpc awsec2.IVpc
	if props.VpcID != "" {
		vpc = awsec2.Vpc_FromLookup(stack, jsii.String("Vpc"), &awsec2.VpcLookupOptions{
			VpcId: jsii.String(props.VpcID),
		})
	} else {
		maxAZs := jsii.Number(2)
		if props.MaxAZs != nil {
			maxAZs = props.MaxAZs
		}
		vpc = awsec2.NewVpc(stack, jsii.String("Vpc"), &awsec2.VpcProps{
			MaxAzs: maxAZs,
		})
	}

	efsConfig := gobridgecdk.NewGoBridgeEfsConfig(stack, jsii.String("EfsConfig"), &gobridgecdk.GoBridgeEfsConfigProps{
		Vpc: vpc,
	})

	gobridgecdk.NewGoBridgeService(stack, jsii.String("Service"), &gobridgecdk.GoBridgeServiceProps{
		Vpc:         vpc,
		ServiceName: props.ServiceName,
		Image:       awsecs.ContainerImage_FromRegistry(jsii.String(props.ImageURI), nil),
		Bootstrap:   props.Bootstrap,
		Exposure:    props.Exposure,
		EfsConfig:   efsConfig,
	})

	return stack
}
