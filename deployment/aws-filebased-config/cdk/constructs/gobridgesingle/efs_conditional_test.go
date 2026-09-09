//go:build !race

package gobridgesingle_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssns"
	"github.com/aws/jsii-runtime-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealarms"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/ssmexports"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

// TestSingle_EFS_Conditional verifies YAML store paths and the runtime source determine filesystem use.
func TestSingle_EFS_Conditional(t *testing.T) {
	for _, tc := range []struct {
		name, source, store string
		wantEFS             bool
	}{
		{name: "default file", wantEFS: true},
		{name: "explicit file", source: "file", wantEFS: true},
		{name: "DynamoDB without SQLite", source: "dynamodb"},
		{name: "DynamoDB with SQLite outbox", source: "dynamodb", store: "outbox", wantEFS: true},
		{name: "DynamoDB with SQLite DLQ", source: "dynamodb", store: "dlq", wantEFS: true},
		{name: "DynamoDB with SQLite subscriptions", source: "dynamodb", store: "managed_subscriptions", wantEFS: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boot := singleBootstrap()
			boot.ConfigSource = tc.source
			if tc.source == infra.ConfigSourceDynamoDB {
				boot.ConfigFilePath = ""
			}
			yaml := singleSampleYAML
			if tc.store != "" {
				yaml += "stores:\n  " + tc.store + ":\n    type: sqlite\n    options:\n      path: /var/lib/gobridge/store.db\n"
			}
			for _, kind := range []string{"asset", "inline"} {
				t.Run(kind, func(t *testing.T) {
					src := source.NewAsset(writeSingleYAML(t, yaml))
					if kind == "inline" {
						mat, err := src.Materialize()
						require.NoError(t, err)
						t.Cleanup(func() { _ = mat.Close() })
						src = source.NewInline(mat.Config)
					}
					stack, g := configSingleStack(t, boot, src)
					tpl := assertions.Template_FromStack(stack, nil)
					count := 0.0
					if tc.wantEFS {
						count = 1
					}
					assert.Equal(t, tc.wantEFS, g.EfsConfig() != nil)
					tpl.ResourceCountIs(jsii.String("AWS::EFS::FileSystem"), jsii.Number(count))
					if !tc.wantEFS {
						tpl.ResourceCountIs(jsii.String("AWS::EFS::AccessPoint"), jsii.Number(0))
						tpl.ResourceCountIs(jsii.String("AWS::EFS::MountTarget"), jsii.Number(0))
					}
					if tc.source != infra.ConfigSourceDynamoDB {
						tpl.ResourceCountIs(jsii.String("AWS::DynamoDB::Table"), jsii.Number(0))
					}
					for _, raw := range *tpl.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil) {
						task := (*raw)["Properties"].(map[string]any)
						if tc.wantEFS {
							assert.Len(t, task["Volumes"], 1)
						} else {
							assert.Empty(t, task["Volumes"])
						}
						assert.Len(t, task["ContainerDefinitions"], 1)
						for _, rawContainer := range task["ContainerDefinitions"].([]any) {
							container := rawContainer.(map[string]any)
							if container["Name"] != "gobridge" {
								continue
							}
							if tc.wantEFS {
								assert.Len(t, container["MountPoints"], 1)
							} else {
								assert.Empty(t, container["MountPoints"])
							}
							assert.Empty(t, container["DependsOn"])
						}
					}
				})
			}
		})
	}
}

// TestSingle_EFSFree_Consumers verifies ALB exports and alarms work when the facade owns no filesystem.
func TestSingle_EFSFree_Consumers(t *testing.T) {
	src := source.NewAsset(writeSingleYAML(t, singleSampleYAML))
	stack, g := configSingleStack(t, dynamoBootstrap(), src)
	alarms := gobridgealarms.NewGoBridgeAlarms(stack, jsii.String("Alarms"), &gobridgealarms.AlarmsProps{
		Single: g, Efs: g.EfsConfig(), AlarmTopic: awssns.NewTopic(stack, jsii.String("Topic"), nil),
	})
	assert.Nil(t, alarms.EfsIOAlarm())
	assert.NotNil(t, alarms.ControlAbsenceAlarm())
	alb := elbv2.NewApplicationLoadBalancer(stack, jsii.String("ALB"), &elbv2.ApplicationLoadBalancerProps{Vpc: g.Cluster().Vpc()})
	listener := alb.AddListener(jsii.String("Listener"), &elbv2.BaseApplicationListenerProps{
		Port: jsii.Number(80), DefaultAction: elbv2.ListenerAction_FixedResponse(jsii.Number(404), nil),
	})
	attachment := gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Attachment"), &gobridgealbattachment.AttachmentProps{
		Single: g, Listener: listener, Vpc: g.Cluster().Vpc(), BridgeConfig: src,
	})
	attachment.WithSSMExports("/test/bridge", ssmexports.IncludeARNs())
	tpl := assertions.Template_FromStack(stack, nil)
	tpl.ResourceCountIs(jsii.String("AWS::SSM::Parameter"), jsii.Number(5))
	for _, raw := range *tpl.FindResources(jsii.String("AWS::SSM::Parameter"), nil) {
		assert.NotEqual(t, "/test/bridge/efs-id", (*raw)["Properties"].(map[string]any)["Name"])
	}
}
