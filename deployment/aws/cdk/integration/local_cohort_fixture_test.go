//go:build integration_local && !race

package integration

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/jsii-runtime-go"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/deployment/aws/infra"
)

// DynamoDB Local retains mirrors between scenarios. A new config-source proof
// must not recover a previous scenario's committed rollout instead of proving
// its own generation-zero baseline, regardless of suite order or repetition.
func TestDynamoDBConfigFixture_IsolatesRolloutBaseline(t *testing.T) {
	env := haSandbox{
		SandboxEnv: SandboxEnv{
			Account: localAccount, Region: localRegion, VpcID: "vpc-fixture",
			AvailabilityZones: []string{"us-east-1a", "us-east-1b"},
			SubnetIDs:         []string{"subnet-private-a", "subnet-private-b"}, PublicSubnetIDs: []string{"subnet-public-a", "subnet-public-b"},
		},
		BrokerURL: "tcp://mosquitto:1883", MQTTClientID: "gobridge-local-ha",
		MQTTCredentialParam: localMQTTParam, AdminParam: localAdminParam, ProbeCIDR: "10.0.0.0/8", PlaintextBroker: true,
	}
	app := awscdk.NewApp(&awscdk.AppProps{Outdir: jsii.String(t.TempDir())})
	seen := map[string]bool{}
	for _, tc := range []struct{ name, source string }{
		{"FileConfig", ""}, {"DynamoDBConfigOne", infra.ConfigSourceDynamoDB}, {"DynamoDBConfigTwo", infra.ConfigSourceDynamoDB},
	} {
		stack := awscdk.NewStack(app, jsii.String(tc.name), &awscdk.StackProps{Env: StackEnv(env.SandboxEnv)})
		env.ConfigSource = tc.source
		env.Image = localRuntimeImageSource(stack)
		fixture := newHAFixture(t, stack, env, staticSlotRoster())
		name := fixture.Bridge.RolloutTableName()
		if tc.source == "" {
			require.Equal(t, "gobridge-ha-integration-rollouts", name, "keep the existing file fixture unchanged")
			require.Nil(t, fixture.Bridge.ConfigTable())
		}
		require.False(t, seen[name], "%s reuses another deployment's rollout baseline in %s", tc.name, name)
		seen[name] = true
	}
}
