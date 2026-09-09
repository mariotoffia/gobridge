//go:build !race

package gobridgecluster_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/imgsource"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgecluster"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/singleton"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
)

// TestCluster_DynamoDBConfig_Rejected verifies the facade rejects DynamoDB even with default topology props.
func TestCluster_DynamoDBConfig_Rejected(t *testing.T) {
	t.Cleanup(singleton.ResetForTest)
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("ClusterConfig"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	require.Panics(t, func() {
		gobridgecluster.NewGoBridgeCluster(stack, jsii.String("Bridge"), &gobridgecluster.ClusterProps{
			Vpc: vpc, Image: imgsource.NewRegistry("gobridge@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
			Bootstrap:    infra.BootstrapConfig{BridgeID: "test", ConfigSource: infra.ConfigSourceDynamoDB, AdminAPIKeyParam: "/test/admin"},
			BridgeConfig: source.NewInline(&ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "test"}}),
		})
	})
}
