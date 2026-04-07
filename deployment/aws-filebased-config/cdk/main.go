package main

import (
	"os"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

func main() {
	defer jsii.Close()

	app := awscdk.NewApp(nil)

	serviceName := envOrDefault("GOBRIDGE_SERVICE_NAME", "gobridge")
	imageURI := envOrDefault("GOBRIDGE_IMAGE_URI", "gobridge:latest")
	stackName := envOrDefault("GOBRIDGE_STACK_NAME", "GoBridge")
	bridgeID := envOrDefault("GOBRIDGE_BRIDGE_ID", "gobridge-main")
	configPath := envOrDefault("GOBRIDGE_CONFIG_PATH", "/mnt/gobridge/bridge.yaml")
	adminKeyParam := envOrDefault("GOBRIDGE_ADMIN_KEY_PARAM", "/gobridge/admin-api-key")
	vpcID := os.Getenv("GOBRIDGE_VPC_ID")

	NewGoBridgeStack(app, stackName, &GoBridgeStackProps{
		StackProps: awscdk.StackProps{
			Env: env(),
		},
		ServiceName: serviceName,
		ImageURI:    imageURI,
		VpcID:       vpcID,
		Bootstrap: infra.BootstrapConfig{
			BridgeID:         bridgeID,
			ConfigFilePath:   configPath,
			AdminAPIKeyParam: adminKeyParam,
		},
		Exposure: infra.Exposure{
			Admin:   true,
			Monitor: true,
		},
	})

	app.Synth(nil)
}

func env() *awscdk.Environment {
	region := os.Getenv("CDK_DEFAULT_REGION")
	account := os.Getenv("CDK_DEFAULT_ACCOUNT")
	if region == "" && account == "" {
		return nil
	}
	return &awscdk.Environment{
		Region:  jsii.String(region),
		Account: jsii.String(account),
	}
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
