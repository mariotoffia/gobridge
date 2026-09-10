//go:build !race

package gobridgedynamodbha_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssns"
	"github.com/aws/jsii-runtime-go"
	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealarms"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	ha "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/ssmexports"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

// TestDynamoDBHA_EFSFree_Consumers verifies the HA fleet, ALB and alarms have no filesystem dependency.
func TestDynamoDBHA_EFSFree_Consumers(t *testing.T) {
	h := newHAHarness(t, func(p *ha.DynamoDBHAProps) {
		p.Bootstrap.ConfigSource = infra.ConfigSourceDynamoDB
		p.Bootstrap.ConfigFilePath = ""
	})
	assert.Nil(t, h.bridge.EfsConfig())
	alarms := gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("Alarms"), &gobridgealarms.AlarmsProps{
		DynamoDBHA: h.bridge, Efs: h.bridge.EfsConfig(), AlarmTopic: awssns.NewTopic(h.stack, jsii.String("Topic"), nil),
	})
	assert.Nil(t, alarms.EfsIOAlarm())
	assert.NotNil(t, alarms.WarmStandbyUnavailableAlarm())
	assert.Len(t, alarms.DynamoDBThrottleAlarms(), 3)
	alb := elbv2.NewApplicationLoadBalancer(h.stack, jsii.String("ALB"), &elbv2.ApplicationLoadBalancerProps{Vpc: h.vpc})
	listener := alb.AddListener(jsii.String("Listener"), &elbv2.BaseApplicationListenerProps{
		Port: jsii.Number(80), DefaultAction: elbv2.ListenerAction_FixedResponse(jsii.Number(404), nil),
	})
	attachment := gobridgealbattachment.NewGoBridgeALBAttachment(h.stack, jsii.String("Attachment"), &gobridgealbattachment.AttachmentProps{
		DynamoDBHA: h.bridge, Listener: listener, Vpc: h.vpc, BridgeConfig: h.source,
	})
	attachment.WithSSMExports("/test/ha", ssmexports.IncludeARNs())
	tpl := assertions.Template_FromStack(h.stack, nil)
	for _, kind := range []string{"AWS::EFS::FileSystem", "AWS::EFS::AccessPoint", "AWS::EFS::MountTarget"} {
		tpl.ResourceCountIs(jsii.String(kind), jsii.Number(0))
	}
	tpl.ResourceCountIs(jsii.String("AWS::SSM::Parameter"), jsii.Number(5))
	for _, raw := range *tpl.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil) {
		props := (*raw)["Properties"].(map[string]any)
		assert.Empty(t, props["Volumes"])
		assert.Len(t, props["ContainerDefinitions"], 1)
		main := mainContainerFromTask(t, *raw)
		assert.Empty(t, main["MountPoints"])
		assert.Empty(t, main["DependsOn"])
	}
}
