//go:build !race

package constructs_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/jsii-runtime-go"

	gobridgecdk "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

func TestGoBridgeService_CreatesFargateServiceWithDefaults(t *testing.T) {
	defer jsii.Close()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("TestStack"), nil)

	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)

	svc := gobridgecdk.NewGoBridgeService(stack, jsii.String("Bridge"), &gobridgecdk.GoBridgeServiceProps{
		Vpc:         vpc,
		ServiceName: "test-bridge",
		Image:       awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap: infra.BootstrapConfig{
			BridgeID:         "bridge-1",
			ConfigFilePath:   "/mnt/gobridge/bridge.yaml",
			AdminAPIKeyParam: "/test/admin-key",
		},
	})

	if svc.Service() == nil {
		t.Error("Service() should not be nil")
	}
	if svc.TaskDefinition() == nil {
		t.Error("TaskDefinition() should not be nil")
	}

	template := assertions.Template_FromStack(stack, nil)

	// Verify ECS service exists
	template.HasResourceProperties(jsii.String("AWS::ECS::Service"), map[string]any{
		"ServiceName": "test-bridge",
	})

	// Verify task definition with default CPU/memory
	template.HasResourceProperties(jsii.String("AWS::ECS::TaskDefinition"), map[string]any{
		"Cpu":    "512",
		"Memory": "1024",
	})

	// Verify CloudWatch log group exists
	template.ResourceCountIs(jsii.String("AWS::Logs::LogGroup"), jsii.Number(1))

	// Verify EFS filesystem is created (default)
	template.ResourceCountIs(jsii.String("AWS::EFS::FileSystem"), jsii.Number(1))
}

func TestGoBridgeService_BootstrapConfigInjectedAsEnvVar(t *testing.T) {
	defer jsii.Close()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("TestStack"), nil)

	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)

	gobridgecdk.NewGoBridgeService(stack, jsii.String("Bridge"), &gobridgecdk.GoBridgeServiceProps{
		Vpc:         vpc,
		ServiceName: "test-bridge",
		Image:       awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap: infra.BootstrapConfig{
			BridgeID:         "env-test",
			ConfigFilePath:   "/mnt/gobridge/bridge.yaml",
			AdminAPIKeyParam: "/test/admin-key",
		},
	})

	template := assertions.Template_FromStack(stack, nil)

	// The bootstrap JSON should contain the bridge_id
	template.HasResourceProperties(jsii.String("AWS::ECS::TaskDefinition"), map[string]any{
		"ContainerDefinitions": []map[string]any{
			{
				"Environment": assertions.Match_ArrayWith(&[]interface{}{
					assertions.Match_ObjectLike(&map[string]interface{}{
						"Name":  "GOBRIDGE_FILEBASED_BOOTSTRAP_JSON",
						"Value": assertions.Match_StringLikeRegexp(jsii.String("env-test")),
					}),
				}),
			},
		},
	})
}

func TestGoBridgeService_CustomCPUAndMemory(t *testing.T) {
	defer jsii.Close()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("TestStack"), nil)

	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)

	gobridgecdk.NewGoBridgeService(stack, jsii.String("Bridge"), &gobridgecdk.GoBridgeServiceProps{
		Vpc:         vpc,
		ServiceName: "test-bridge",
		Image:       awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		CPU:         jsii.Number(1024),
		MemoryMiB:   jsii.Number(2048),
		Bootstrap: infra.BootstrapConfig{
			BridgeID:         "bridge-1",
			ConfigFilePath:   "/mnt/gobridge/bridge.yaml",
			AdminAPIKeyParam: "/test/admin-key",
		},
	})

	template := assertions.Template_FromStack(stack, nil)
	template.HasResourceProperties(jsii.String("AWS::ECS::TaskDefinition"), map[string]any{
		"Cpu":    "1024",
		"Memory": "2048",
	})
}

func TestGoBridgeService_ExposureAddsPortMappings(t *testing.T) {
	defer jsii.Close()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("TestStack"), nil)

	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)

	gobridgecdk.NewGoBridgeService(stack, jsii.String("Bridge"), &gobridgecdk.GoBridgeServiceProps{
		Vpc:         vpc,
		ServiceName: "test-bridge",
		Image:       awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap: infra.BootstrapConfig{
			BridgeID:         "bridge-1",
			ConfigFilePath:   "/mnt/gobridge/bridge.yaml",
			AdminAPIKeyParam: "/test/admin-key",
		},
		Exposure: infra.Exposure{
			Admin:         true,
			Monitor:       true,
			TransportHTTP: true,
		},
	})

	template := assertions.Template_FromStack(stack, nil)

	// Verify container has port mappings for all three ports
	template.HasResourceProperties(jsii.String("AWS::ECS::TaskDefinition"), map[string]any{
		"ContainerDefinitions": []map[string]any{
			{
				"PortMappings": assertions.Match_ArrayWith(&[]interface{}{
					assertions.Match_ObjectLike(&map[string]interface{}{
						"ContainerPort": 8080,
					}),
					assertions.Match_ObjectLike(&map[string]interface{}{
						"ContainerPort": 8081,
					}),
					assertions.Match_ObjectLike(&map[string]interface{}{
						"ContainerPort": 8082,
					}),
				}),
			},
		},
	})
}
