package main

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

func TestGoBridgeStack_CreatesFullStack(t *testing.T) {
	defer jsii.Close()
	app := awscdk.NewApp(nil)

	stack := NewGoBridgeStack(app, "TestStack", &GoBridgeStackProps{
		ServiceName: "my-bridge",
		ImageURI:    "gobridge:latest",
		Bootstrap: infra.BootstrapConfig{
			BridgeID:         "stack-test-bridge",
			ConfigFilePath:   "/mnt/gobridge/bridge.yaml",
			AdminAPIKeyParam: "/gobridge/admin-key",
		},
		Exposure: infra.Exposure{
			Admin:   true,
			Monitor: true,
		},
	})

	template := assertions.Template_FromStack(stack, nil)

	// Verify VPC is created
	template.ResourceCountIs(jsii.String("AWS::EC2::VPC"), jsii.Number(1))

	// Verify EFS filesystem
	template.ResourceCountIs(jsii.String("AWS::EFS::FileSystem"), jsii.Number(1))

	// Verify ECS service
	template.HasResourceProperties(jsii.String("AWS::ECS::Service"), map[string]any{
		"ServiceName": "my-bridge",
	})

	// Verify task definition
	template.HasResourceProperties(jsii.String("AWS::ECS::TaskDefinition"), map[string]any{
		"Cpu":    "512",
		"Memory": "1024",
	})
}
