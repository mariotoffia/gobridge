//go:build (integration_aws || integration_local) && !race

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/jsii-runtime-go"
	"github.com/stretchr/testify/require"
)

func TestCredentialedFixtures_StageEmbeddedRuntimeConfig(t *testing.T) {
	t.Setenv("GOBRIDGE_INT_IMAGE", "")
	t.Setenv("GOBRIDGE_INT_VERSION", "v0.0.0")
	env := SandboxEnv{
		Account: "111122223333", Region: "us-east-1", VpcID: "vpc-12345678",
		AvailabilityZones: []string{"us-east-1a", "us-east-1b"},
		SubnetIDs:         []string{"subnet-12345678", "subnet-23456789"}, PublicSubnetIDs: []string{"subnet-34567890", "subnet-45678901"},
	}
	for _, tc := range []struct {
		name  string
		build func(awscdk.Stack, SandboxEnv) integrationFixture
	}{{"Single", newSingleFixture}, {"Cluster", newClusterFixture}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			app := awscdk.NewApp(&awscdk.AppProps{Outdir: jsii.String(dir)})
			stack := awscdk.NewStack(app, jsii.String(tc.name), &awscdk.StackProps{Env: StackEnv(env)})
			tc.build(stack, env)
			app.Synth(nil)
			data, err := os.ReadFile(filepath.Join(dir, tc.name+".assets.json"))
			require.NoError(t, err)
			var assets struct{ DockerImages map[string]any }
			require.NoError(t, json.Unmarshal(data, &assets))
			require.NotEmpty(t, assets.DockerImages, "each fixture must embed its own config in a build asset")
			data, err = os.ReadFile(filepath.Join(dir, tc.name+".template.json"))
			require.NoError(t, err)
			var template struct {
				Resources map[string]struct {
					Type       string
					Properties struct{ ContainerDefinitions []struct{ Name string } }
				}
			}
			require.NoError(t, json.Unmarshal(data, &template))
			for _, resource := range template.Resources {
				if resource.Type == "AWS::ECS::TaskDefinition" {
					require.Len(t, resource.Properties.ContainerDefinitions, 1)
					require.Equal(t, "gobridge", resource.Properties.ContainerDefinitions[0].Name)
				}
			}
		})
	}
}
