//go:build integration_local

package integration

import (
	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecr"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/gobridgecdk"
)

// The facade synthesizes the same ECR image/pull-grant shape used on AWS.
// Only the emulator's assembly rewrite substitutes the locally built image,
// which has no registry manifest digest because it has never been pushed.
//
//nolint:ireturn // BridgeImageSource is the sealed facade input.
func localRuntimeImageSource(stack awscdk.Stack) gobridgecdk.BridgeImageSource {
	repo := awsecr.Repository_FromRepositoryName(stack, jsii.String("RuntimeImageRepository"), jsii.String("gobridge-local"))
	return gobridgecdk.ImageFromEcrRepository(repo, "local")
}

func useLocalRuntimeImage(properties map[string]any) {
	image := localBridgeImage()
	containers, _ := properties["ContainerDefinitions"].([]any)
	for _, value := range containers {
		container, ok := value.(map[string]any)
		if ok && container["Name"] == "gobridge" {
			container["Image"] = image
		}
	}
}
