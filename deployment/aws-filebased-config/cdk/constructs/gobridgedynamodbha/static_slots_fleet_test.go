//go:build !race

package gobridgedynamodbha_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssns"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealarms"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	ha "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha"
)

// Fleet coverage. A member slot is an ECS service like any other, so anything the
// facade hands to the ALB attachment or the alarm construct must name EVERY slot.
// Covering only the first would leave the remaining slots serving ingress that
// never reaches them, and sitting at zero tasks with every alarm green.

// staticSlotYAMLWithHTTPReceiver adds an HTTP receiver to the coordinated config.
// Without one the attachment builds NO transport target group at all
// (deriveReceiverPaths is empty), so a test that omits it never executes the
// multi-slot registration it claims to cover.
func staticSlotYAMLWithHTTPReceiver(t *testing.T) string {
	t.Helper()
	const anchor = "receivers:\n"
	const httpReceiver = "receivers:\n" +
		"  - id: webhook\n" +
		"    transport: http\n" +
		"    options:\n" +
		"      path: /hooks/webhook\n"
	yaml := withClusterBlock(t, staticSlotClusterYAML(controlSlotID, workerSlotA, workerSlotB))
	out := strings.Replace(yaml, anchor, httpReceiver, 1)
	if out == yaml {
		t.Fatalf("HTTP receiver anchor %q no longer appears in the harness config", anchor)
	}
	return out
}

func TestGoBridgeDynamoDBHA_ALBAttachmentTargetsEveryMemberSlot(t *testing.T) {
	h := newHAHarnessWithYAML(t, staticSlotYAMLWithHTTPReceiver(t), func(props *ha.DynamoDBHAProps) {
		props.MemberSlots = defaultMemberSlots()
	})
	alb := elbv2.NewApplicationLoadBalancer(h.stack, jsii.String("ALB"), &elbv2.ApplicationLoadBalancerProps{Vpc: h.vpc})
	listener := alb.AddListener(jsii.String("Listener"), &elbv2.BaseApplicationListenerProps{
		Port:          jsii.Number(80),
		DefaultAction: elbv2.ListenerAction_FixedResponse(jsii.Number(404), nil),
	})
	gobridgealbattachment.NewGoBridgeALBAttachment(h.stack, jsii.String("Attachment"), &gobridgealbattachment.AttachmentProps{
		DynamoDBHA:   h.bridge,
		Listener:     listener,
		Vpc:          h.vpc,
		BridgeConfig: h.source,
	})

	template := assertions.Template_FromStack(h.stack, nil)
	groups := *template.FindResources(jsii.String("AWS::ElasticLoadBalancingV2::TargetGroup"), nil)

	// Two distinct groups matter here. The MONITOR group is the one every service
	// joins: it load-balances "/api/v1/monitor/*" and makes CDK open the monitor
	// port on each slot's security group, without which the port-overridden health
	// checks on the other groups are unreachable. The TRANSPORT (worker) group is
	// where HTTP receiver ingress lands.
	monitorTG, transportTG := "", ""
	for logicalID, raw := range groups {
		port, _ := (*raw)["Properties"].(map[string]any)["Port"].(float64)
		switch port {
		case 8081:
			monitorTG = logicalID
		case 8082:
			transportTG = logicalID
		}
	}
	if monitorTG == "" || transportTG == "" {
		t.Fatalf("target groups = %v, want a monitor group on 8081 and a transport group on 8082", groups)
	}

	registered := map[string]map[string]bool{}
	for logicalID, raw := range *template.FindResources(jsii.String("AWS::ECS::Service"), nil) {
		balancers, _ := (*raw)["Properties"].(map[string]any)["LoadBalancers"].([]any)
		if len(balancers) == 0 {
			t.Fatalf("%s is registered with no ALB target group: its tasks would never receive traffic "+
				"and its security group would never open the monitor port", logicalID)
		}
		joined := map[string]bool{}
		for _, entry := range balancers {
			body, err := json.Marshal(entry)
			if err != nil {
				t.Fatalf("marshal load balancer entry: %v", err)
			}
			for _, group := range []string{monitorTG, transportTG} {
				if strings.Contains(string(body), group) {
					joined[group] = true
				}
			}
		}
		registered[logicalID] = joined
	}
	if len(registered) != 3 {
		t.Fatalf("ALB-registered services = %d, want the control slot plus both worker slots", len(registered))
	}

	workerSlots := 0
	for logicalID, joined := range registered {
		if !joined[monitorTG] {
			t.Fatalf("%s is not in the monitor target group, so the ALB cannot reach its monitor port",
				logicalID)
		}
		if strings.Contains(logicalID, "ControlService") {
			continue
		}
		workerSlots++
		if !joined[transportTG] {
			t.Fatalf("%s is not in the transport target group: that slot would run, report healthy, and "+
				"never receive the HTTP receiver ingress it is provisioned to serve", logicalID)
		}
	}
	if workerSlots != 2 {
		t.Fatalf("worker slot services registered on the transport group = %d, want 2", workerSlots)
	}
}

func TestGoBridgeDynamoDBHA_WorkerAlarmsCoverEveryMemberSlot(t *testing.T) {
	h := newStaticSlotHarness(t, nil)
	topic := awssns.NewTopic(h.stack, jsii.String("AlarmTopic"), nil)
	alarms := gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("Alarms"), &gobridgealarms.AlarmsProps{
		DynamoDBHA: h.bridge, Efs: h.bridge.EfsConfig(), AlarmTopic: topic,
	})
	if got := len(alarms.WorkerDegradedAlarms()); got != 2 {
		t.Fatalf("worker capacity alarms = %d, want one per slot so the alarm names the slot that is "+
			"short a task", got)
	}
	template := assertions.Template_FromStack(h.stack, nil)

	// The ServiceName dimension resolves to Fn::GetAtt on the service resource, so
	// a slot is identified inside an alarm by its CloudFormation logical id.
	workerSlotIDs := map[string]bool{}
	for logicalID := range *template.FindResources(jsii.String("AWS::ECS::Service"), nil) {
		if strings.Contains(logicalID, "WorkerService") {
			workerSlotIDs[logicalID] = true
		}
	}
	if len(workerSlotIDs) != 2 {
		t.Fatalf("worker slot services = %v, want two", workerSlotIDs)
	}

	degradedSlots := map[string]bool{}
	warmStandby := ""
	for _, raw := range *template.FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil) {
		props := (*raw)["Properties"].(map[string]any)
		description, _ := props["AlarmDescription"].(string)
		body, err := json.Marshal(props["Metrics"])
		if err != nil {
			t.Fatalf("marshal alarm metrics: %v", err)
		}
		switch {
		case strings.Contains(description, "worker service running task count"):
			// One alarm per slot: exactly one slot per alarm, or the alarm cannot name
			// the slot that broke.
			named := ""
			for slotID := range workerSlotIDs {
				if strings.Contains(string(body), slotID) {
					if named != "" {
						t.Fatalf("a worker capacity alarm observes both %s and %s; per-slot alarms exist so "+
							"the alarm names the failing slot", named, slotID)
					}
					named = slotID
				}
			}
			if named == "" {
				t.Fatalf("a worker capacity alarm observes no slot service: %s", body)
			}
			degradedSlots[named] = true
		case strings.Contains(description, "fewer than two running tasks"):
			warmStandby = string(body)
		}
	}

	for slotID := range workerSlotIDs {
		if !degradedSlots[slotID] {
			t.Fatalf("slot %s has no capacity alarm; it could sit at zero tasks while every alarm "+
				"stays green", slotID)
		}
	}
	if warmStandby == "" {
		t.Fatal("no warm-standby alarm was installed for the coordinated HA fleet")
	}
	// The warm-standby alarm is the one that must still SUM: "is anybody left" is a
	// fleet-wide question, not a per-slot one.
	for slotID := range workerSlotIDs {
		if !strings.Contains(warmStandby, slotID) {
			t.Fatalf("the warm-standby alarm does not count slot %s, so the fleet could drop below a "+
				"warm standby without breaching", slotID)
		}
	}
	// json.Marshal escapes "<" as \u003c, so compare on the decoded text.
	if want := "IF(control + wr0 + wr1 < 2, 1, 0)"; !strings.Contains(
		strings.ReplaceAll(warmStandby, `\u003c`, "<"), want) {
		t.Fatalf("warm-standby expression does not sum every slot (want %q): %s", want, warmStandby)
	}
}

// The rollout coordination table is the barrier's only shared store AND the gate
// a booting member must read, so throttling on it both stalls every rollout and
// can stop a replaced slot from starting. It gets the same throttle and
// system-error alarms as the other three deployment-owned tables.
func TestGoBridgeDynamoDBHA_RolloutTableIsAlarmedLikeEveryOtherOwnedTable(t *testing.T) {
	h := newStaticSlotHarness(t, nil)
	topic := awssns.NewTopic(h.stack, jsii.String("AlarmTopic"), nil)
	gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("Alarms"), &gobridgealarms.AlarmsProps{
		DynamoDBHA: h.bridge, Efs: h.bridge.EfsConfig(), AlarmTopic: topic,
	})
	template := assertions.Template_FromStack(h.stack, nil)

	rolloutAlarms := 0
	for logicalID := range *template.FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil) {
		if strings.Contains(logicalID, "Rollout") &&
			(strings.Contains(logicalID, "Throttle") || strings.Contains(logicalID, "SystemError")) {
			rolloutAlarms++
		}
	}
	if rolloutAlarms != 2 {
		t.Fatalf("rollout table throttle/system-error alarms = %d, want 2", rolloutAlarms)
	}

	// The autoscaled profile provisions no rollout table, so it must install
	// neither alarm rather than one that can never leave INSUFFICIENT_DATA.
	auto := newHAHarness(t, nil)
	autoTopic := awssns.NewTopic(auto.stack, jsii.String("AlarmTopic"), nil)
	gobridgealarms.NewGoBridgeAlarms(auto.stack, jsii.String("Alarms"), &gobridgealarms.AlarmsProps{
		DynamoDBHA: auto.bridge, Efs: auto.bridge.EfsConfig(), AlarmTopic: autoTopic,
	})
	for logicalID := range *assertions.Template_FromStack(auto.stack, nil).
		FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil) {
		if strings.Contains(logicalID, "RolloutThrottle") || strings.Contains(logicalID, "RolloutSystemError") {
			t.Fatalf("autoscaled profile installed %s but provisions no rollout table", logicalID)
		}
	}
}
