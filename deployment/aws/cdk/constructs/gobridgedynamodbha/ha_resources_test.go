//go:build !race

package gobridgedynamodbha_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssns"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgealarms"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgealbattachment"
)

func TestGoBridgeDynamoDBHA_TaskRolesHaveExactDynamoDBDataPlaneGrants(t *testing.T) {
	h := newHAHarness(t, nil)
	template := assertions.Template_FromStack(h.stack, nil)
	policies := template.FindResources(jsii.String("AWS::IAM::Policy"), nil)
	type roleGrant struct {
		actions   map[string]bool
		resources string
	}
	grantsByRole := map[string]*roleGrant{}
	for _, raw := range *policies {
		props := (*raw)["Properties"].(map[string]any)
		role := ""
		for _, roleRaw := range props["Roles"].([]any) {
			ref, _ := roleRaw.(map[string]any)["Ref"].(string)
			switch {
			case strings.Contains(ref, "Control"):
				role = "control"
			case strings.Contains(ref, "Worker"):
				role = "worker"
			}
		}
		if role == "" {
			continue
		}
		document, _ := props["PolicyDocument"].(map[string]any)
		for _, statementRaw := range document["Statement"].([]any) {
			statement := statementRaw.(map[string]any)
			actions := normalizeActions(statement["Action"])
			hasDynamoDB := false
			for _, action := range actions {
				if strings.HasPrefix(action, "dynamodb:") {
					hasDynamoDB = true
				}
			}
			if !hasDynamoDB {
				continue
			}
			if grantsByRole[role] == nil {
				grantsByRole[role] = &roleGrant{actions: map[string]bool{}}
			}
			for _, action := range actions {
				grantsByRole[role].actions[action] = true
			}
			rawResource, err := json.Marshal(statement["Resource"])
			if err != nil {
				t.Fatalf("marshal IAM resource: %v", err)
			}
			grantsByRole[role].resources += string(rawResource)
		}
	}
	want := map[string]bool{
		"dynamodb:GetItem": true, "dynamodb:PutItem": true,
		"dynamodb:UpdateItem": true, "dynamodb:Query": true,
		"dynamodb:TransactWriteItems": true,
		"dynamodb:DescribeTable":      true, "dynamodb:DescribeTimeToLive": true,
	}
	for _, role := range []string{"control", "worker"} {
		grant := grantsByRole[role]
		if grant == nil {
			t.Fatalf("no DynamoDB grants found for %s task role", role)
		}
		if fmt.Sprint(grant.actions) != fmt.Sprint(want) {
			for action := range grant.actions {
				if !want[action] {
					t.Errorf("%s role has forbidden DynamoDB action %s", role, action)
				}
			}
			for action := range want {
				if !grant.actions[action] {
					t.Errorf("%s role missing DynamoDB action %s", role, action)
				}
			}
		}
		for _, table := range []string{"gobridge-leases", "gobridge-outbox", "gobridge-managed-subscriptions"} {
			if !strings.Contains(grant.resources, table) {
				t.Errorf("%s role resources do not contain exact table %s: %s", role, table, grant.resources)
			}
		}
		for _, index := range []string{"ExpiryIndex", "RecordIDIndex", "ClaimIndex"} {
			if !strings.Contains(grant.resources, "/index/"+index) {
				t.Errorf("%s role resources do not contain exact index %s", role, index)
			}
		}
		if strings.Contains(grant.resources, "/index/*") || strings.Contains(grant.resources, "resource/*") {
			t.Errorf("%s role has wildcard DynamoDB resources: %s", role, grant.resources)
		}
	}
}

func normalizeActions(raw any) []string {
	switch value := raw.(type) {
	case string:
		return []string{value}
	case []any:
		out := make([]string, 0, len(value))
		for _, entry := range value {
			if action, ok := entry.(string); ok {
				out = append(out, action)
			}
		}
		return out
	default:
		return nil
	}
}

func TestGoBridgeDynamoDBHA_HealthyStandbyDoesNotInstallAcquireContentionAlarm(t *testing.T) {
	h := newHAHarness(t, nil)
	topic := awssns.NewTopic(h.stack, jsii.String("AlarmTopic"), nil)
	gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("Alarms"), &gobridgealarms.AlarmsProps{
		DynamoDBHA: h.bridge, Efs: h.bridge.EfsConfig(), AlarmTopic: topic,
	})
	alarms := assertions.Template_FromStack(h.stack, nil).FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil)
	for logicalID, raw := range *alarms {
		properties := (*raw)["Properties"].(map[string]any)
		if properties["MetricName"] == "LeaseAcquireFailures" || strings.Contains(logicalID, "HALeaseAcquireFailures") {
			t.Fatalf("healthy warm-standby contention must not install LeaseAcquireFailures alarm: %s %v", logicalID, properties)
		}
	}
}

func TestGoBridgeDynamoDBHA_ALBAttachmentTargetsHAServiceSet(t *testing.T) {
	h := newHAHarness(t, nil)
	alb := elbv2.NewApplicationLoadBalancer(h.stack, jsii.String("ALB"), &elbv2.ApplicationLoadBalancerProps{Vpc: h.vpc})
	listener := alb.AddListener(jsii.String("Listener"), &elbv2.BaseApplicationListenerProps{
		Port:          jsii.Number(80),
		DefaultAction: elbv2.ListenerAction_FixedResponse(jsii.Number(404), nil),
	})
	attachment := gobridgealbattachment.NewGoBridgeALBAttachment(h.stack, jsii.String("Attachment"), &gobridgealbattachment.AttachmentProps{
		DynamoDBHA:   h.bridge,
		Listener:     listener,
		Vpc:          h.vpc,
		BridgeConfig: h.source,
	})
	if attachment.ControlTargetGroup() == nil || attachment.MonitorTargetGroup() == nil {
		t.Fatal("HA attachment target groups are nil")
	}
}

func TestGoBridgeDynamoDBHA_AlarmsCoverHAAndExternalDuration(t *testing.T) {
	h := newHAHarness(t, nil)
	topic := awssns.NewTopic(h.stack, jsii.String("AlarmTopic"), nil)
	alarms := gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("Alarms"), &gobridgealarms.AlarmsProps{
		DynamoDBHA: h.bridge,
		Efs:        h.bridge.EfsConfig(),
		AlarmTopic: topic,
	})
	if alarms.WarmStandbyUnavailableAlarm() == nil || alarms.FailureToFullDurationAlarm() == nil {
		t.Fatal("warm-standby or failure-to-Full alarm is nil")
	}

	template := assertions.Template_FromStack(h.stack, nil)
	resources := template.FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil)
	metricNames := map[string]bool{}
	foundDuration := false
	for _, raw := range *resources {
		props := (*raw)["Properties"].(map[string]any)
		if name, ok := props["MetricName"].(string); ok {
			metricNames[name] = true
			if name == gobridgealarms.FailureToFullMetricName {
				foundDuration = true
				if props["TreatMissingData"] != "notBreaching" {
					t.Fatalf("FailureToFullDuration TreatMissingData = %v, want notBreaching", props["TreatMissingData"])
				}
				if props["Threshold"] != float64(120000) {
					t.Fatalf("FailureToFullDuration threshold = %v, want 120000ms", props["Threshold"])
				}
			}
		}
		if metrics, ok := props["Metrics"].([]any); ok {
			for _, metric := range metrics {
				m, _ := metric.(map[string]any)
				stat, _ := m["MetricStat"].(map[string]any)
				md, _ := stat["Metric"].(map[string]any)
				name, _ := md["MetricName"].(string)
				if name != "" {
					metricNames[name] = true
				}
			}
		}
	}
	if !foundDuration {
		t.Fatal("FailureToFullDuration alarm not found")
	}
	for _, name := range []string{
		"RunningTaskCount", "DesiredTaskCount", "SystemErrors", "ThrottledRequests",
		"LeaseExpiries", "LeaseTransfers",
		"OutboxDepth", "OutboxDrainLatency", "OutboxRecordFailures",
		"DLQDepth", "DLQEntries", "DLQWriteFailures",
	} {
		if !metricNames[name] {
			t.Errorf("missing HA alarm metric %q; got %v", name, metricNames)
		}
	}
}
