//go:build !race

package gobridgedynamodbha_test

import (
	"fmt"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/jsii-runtime-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/bridge"
	ha "github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgedynamodbha"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws/infra"
)

// Both targets assign the initial stored version independently of the embedded
// document. The deployment baseline identifies content, not that target counter.
func TestDynamoDBHA_BaselineDigest_SourceVersionIdentity(t *testing.T) {
	for _, configSource := range []string{infra.ConfigSourceFile, infra.ConfigSourceDynamoDB} {
		t.Run(configSource, func(t *testing.T) {
			var unversionedDigest string
			for _, version := range []int{0, 1, 99} {
				yaml := withClusterBlock(t, staticSlotClusterYAML(controlSlotID, workerSlotA, workerSlotB))
				if version != 0 {
					yaml += fmt.Sprintf("\nversion: %d\n", version)
				}
				h := newHAHarnessWithYAML(t, yaml, func(props *ha.DynamoDBHAProps) {
					props.MemberSlots = defaultMemberSlots()
					props.Bootstrap.ConfigSource = configSource
				})
				mat, err := h.source.Materialize()
				require.NoError(t, err)
				t.Cleanup(func() { _ = mat.Close() })
				require.Equal(t, version, mat.Config.Version)
				fullDigest, err := bridge.ConfigArtifactDigest(mat.Config)
				require.NoError(t, err)
				contentDigest, err := bridge.DeploymentBaselineContentDigest(mat.Config)
				require.NoError(t, err)
				tasks := assertions.Template_FromStack(h.stack, nil).
					FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
				require.Len(t, *tasks, 3)
				for slot, raw := range *tasks {
					cfg := haConfigBootstrap(t, mainContainerFromTask(t, *raw))
					if version == 0 {
						unversionedDigest = cfg.DynamoDBHABaselineConfigDigest
					}
					assert.Equal(t, contentDigest, cfg.DynamoDBHABaselineConfigDigest,
						"CDK and runtime must use the same content identity")
					assert.Equal(t, unversionedDigest, cfg.DynamoDBHABaselineConfigDigest,
						"slot %s must recognize deployment content at YAML version %d", slot, version)
					if version != 0 {
						assert.NotEqual(t, fullDigest, contentDigest,
							"committed artifacts must still identify their actual target version")
					}
				}
			}
		})
	}
}

func TestDynamoDBHA_BaselineDigest_IncludesEditableContent(t *testing.T) {
	var baseline, profile string
	for _, logLevel := range []string{"info", "debug"} {
		h := newStaticSlotHarness(t, func(props *ha.DynamoDBHAProps) {
			props.Bootstrap.ConfigSource = infra.ConfigSourceDynamoDB
			mat, err := props.BridgeConfig.Materialize()
			require.NoError(t, err)
			t.Cleanup(func() { _ = mat.Close() })
			mat.Config.Version = 99
			mat.Config.Bridge.LogLevel = logLevel
			props.BridgeConfig = source.NewInline(mat.Config)
		})
		tasks := assertions.Template_FromStack(h.stack, nil).
			FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
		require.Len(t, *tasks, 3)
		for _, raw := range *tasks {
			cfg := haConfigBootstrap(t, mainContainerFromTask(t, *raw))
			if logLevel == "info" {
				baseline, profile = cfg.DynamoDBHABaselineConfigDigest, cfg.DynamoDBHAConfigFingerprint
			} else {
				assert.NotEqual(t, baseline, cfg.DynamoDBHABaselineConfigDigest)
				assert.Equal(t, profile, cfg.DynamoDBHAConfigFingerprint,
					"editable content moves the baseline, not deployment admission")
			}
		}
	}
}
