//go:build !race

package gobridgedynamodbha_test

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssns"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealarms"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	ha "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
)

// What an operator gets for free, and what they must still author themselves, is
// the first question asked in an incident and the last one anybody can answer
// from source. The published inventory was wrong in the expensive direction: it
// named four alarms while the bundle provisions the better part of thirty, so
// every MQTT, DynamoDB, store-health and fleet-convergence alarm read as
// hand-authored work an operator had to remember to do.
//
// The inventory is therefore compared against the SYNTHESIZED template rather
// than against the construct source: an alarm that is created but never reaches
// CloudFormation protects nothing, and a row describing one that no longer
// synthesizes sends an operator to a console page that does not exist.
//
// Rows are keyed by the alarm's construct id — the identity CDK also builds the
// physical alarm name from, so it is what an operator reads in the console.

const alarmInventoryDoc = "../../../../../docs/aws-deployment/alarms.md"

const alarmInventoryHeading = "## Alarms the CDK bundle provisions"

// alarmInventoryRow matches a row of the inventory table, whose first cell is the
// backticked construct id and whose second names what provisions it.
var alarmInventoryRow = regexp.MustCompile("^\\|\\s*`([A-Za-z0-9]+)`\\s*\\|\\s*([^|]+?)\\s*\\|")

// alarmMissingDataCell picks the "Missing data" column — the fifth — out of an
// inventory row.
var alarmMissingDataCell = regexp.MustCompile("^\\|\\s*`([A-Za-z0-9]+)`\\s*\\|[^|]*\\|[^|]*\\|[^|]*\\|\\s*(?:\\*\\*)?([a-z ]+?)(?:\\*\\*)?\\s*\\|")

// cdkLogicalIDHash is the eight-hex-character uniquifier CDK appends to a
// construct path when it derives a logical id.
var cdkLogicalIDHash = regexp.MustCompile(`[0-9A-F]{8}$`)

// cdkSiblingSuffix is the ordinal CDK appends when the same construct id is
// created more than once in one scope. The bundle does that deliberately — one
// worker-degraded alarm per worker service — so the ordinal is dropped and the
// family is documented once, as the page describes it.
var cdkSiblingSuffix = regexp.MustCompile(`[0-9]+$`)

// documentedAlarms returns the construct id of every alarm the inventory lists,
// mapped to the shape the page says provisions it.
func documentedAlarms(t *testing.T) map[string]string {
	t.Helper()
	body, err := os.ReadFile(alarmInventoryDoc)
	if err != nil {
		t.Fatalf("the alarm inventory page must exist: %v", err)
	}

	out := map[string]string{}
	inSection, inFence := false, false
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, alarmInventoryHeading) {
			inSection = true
			continue
		}
		if !inSection {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			break // the next top-level section ends the inventory
		}
		if match := alarmInventoryRow.FindStringSubmatch(line); match != nil {
			if _, dup := out[match[1]]; dup {
				t.Fatalf("alarm %q is listed twice in %s", match[1], alarmInventoryDoc)
			}
			out[match[1]] = match[2]
		}
	}
	if len(out) == 0 {
		t.Fatalf("no alarm rows parsed from the %q section of %s — the table shape changed",
			alarmInventoryHeading, alarmInventoryDoc)
	}
	return out
}

// synthesizedAlarms returns the construct id of every alarm in the stack's
// template: the logical id with the construct prefix and CDK's hash removed.
func synthesizedAlarms(t *testing.T, stack awscdk.Stack, prefix string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	found := assertions.Template_FromStack(stack, nil).
		FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil)
	if found == nil {
		return out
	}
	for logicalID := range *found {
		id := cdkLogicalIDHash.ReplaceAllString(logicalID, "")
		id = cdkSiblingSuffix.ReplaceAllString(strings.TrimPrefix(id, prefix), "")
		if id == "" {
			t.Fatalf("logical id %q did not reduce to a construct id", logicalID)
		}
		out[id] = true
	}
	return out
}

// everyShapeStack synthesizes every alarm the bundle can create: the
// DynamoDB-coordinated HA profile with static member slots (so the rollout table,
// and its store alarms, exist), an ALB attachment, the fleet convergence alarms,
// and — on a second stack, because the rollup branch is the one shape that
// requires no HA construct — the runtime rollup alarms.
func everyShapeStack(t *testing.T) (haStack awscdk.Stack, rollupStack awscdk.Stack) {
	t.Helper()
	h := newHAHarnessWithYAML(t, staticSlotYAMLWithHTTPReceiver(t), func(props *ha.DynamoDBHAProps) {
		props.MemberSlots = defaultMemberSlots()
	})
	alb := elbv2.NewApplicationLoadBalancer(h.stack, jsii.String("ALB"), &elbv2.ApplicationLoadBalancerProps{Vpc: h.vpc})
	listener := alb.AddListener(jsii.String("Listener"), &elbv2.BaseApplicationListenerProps{
		Port:          jsii.Number(80),
		DefaultAction: elbv2.ListenerAction_FixedResponse(jsii.Number(404), nil),
	})
	attachment := gobridgealbattachment.NewGoBridgeALBAttachment(h.stack, jsii.String("Attachment"),
		&gobridgealbattachment.AttachmentProps{
			DynamoDBHA:   h.bridge,
			Listener:     listener,
			Vpc:          h.vpc,
			BridgeConfig: h.source,
		})
	gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("Alarms"), &gobridgealarms.AlarmsProps{
		DynamoDBHA:                 h.bridge,
		Efs:                        h.bridge.EfsConfig(),
		Attachment:                 attachment,
		AlarmTopic:                 awssns.NewTopic(h.stack, jsii.String("AlarmTopic"), nil),
		EnableClusterRolloutAlarms: true,
	})

	rollup := awscdk.NewStack(h.app, jsii.String("RollupStack"), nil)
	vpc := awsec2.NewVpc(rollup, jsii.String("Vpc"), &awsec2.VpcProps{MaxAzs: jsii.Number(2)})
	single := gobridgesingle.NewGoBridgeSingle(rollup, jsii.String("Single"), &gobridgesingle.SingleProps{
		Vpc:          vpc,
		Image:        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:test"), nil),
		Bootstrap:    haBootstrap(),
		BridgeConfig: h.source,
	})
	gobridgealarms.NewGoBridgeAlarms(rollup, jsii.String("Alarms"), &gobridgealarms.AlarmsProps{
		Single:             single,
		Efs:                single.EfsConfig(),
		AlarmTopic:         awssns.NewTopic(rollup, jsii.String("AlarmTopic"), nil),
		EnableRollupAlarms: true,
	})
	return h.stack, rollup
}

func TestAlarmInventory_DocumentsEverySynthesizedAlarm(t *testing.T) {
	haStack, rollupStack := everyShapeStack(t)
	documented := documentedAlarms(t)

	synthesized := synthesizedAlarms(t, haStack, "Alarms")
	for id := range synthesizedAlarms(t, rollupStack, "Alarms") {
		synthesized[id] = true
	}

	var missing []string
	for id := range synthesized {
		if _, ok := documented[id]; !ok {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("alarms the bundle synthesizes with no row in %s: %s",
			alarmInventoryDoc, strings.Join(missing, ", "))
	}
}

func TestAlarmInventory_DocumentsNoAlarmTheBundleDoesNotCreate(t *testing.T) {
	haStack, rollupStack := everyShapeStack(t)

	synthesized := synthesizedAlarms(t, haStack, "Alarms")
	for id := range synthesizedAlarms(t, rollupStack, "Alarms") {
		synthesized[id] = true
	}

	var invented []string
	for id := range documentedAlarms(t) {
		if !synthesized[id] {
			invented = append(invented, id)
		}
	}
	sort.Strings(invented)
	if len(invented) > 0 {
		t.Fatalf("%s lists alarms no shape of the bundle synthesizes: %s",
			alarmInventoryDoc, strings.Join(invented, ", "))
	}
}

const rollupRequirementHeading = "## Rollup metrics the built-in alarms require"

// documentedRollupMetrics returns the metrics the page says the exporter must
// publish a dimensionless copy of.
func documentedRollupMetrics(t *testing.T) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(alarmInventoryDoc)
	if err != nil {
		t.Fatalf("the alarm inventory page must exist: %v", err)
	}

	out := map[string]bool{}
	inSection, inFence := false, false
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, rollupRequirementHeading) {
			inSection = true
			continue
		}
		if !inSection {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			break
		}
		if match := alarmInventoryRow.FindStringSubmatch(line); match != nil {
			out[match[1]] = true
		}
	}
	if len(out) == 0 {
		t.Fatalf("no rollup metrics parsed from the %q section of %s",
			rollupRequirementHeading, alarmInventoryDoc)
	}
	return out
}

// dimensionlessRuntimeAlarms returns the runtime metric each alarm reads with NO
// dimensions. Those alarms can only ever match a rollup copy.
func dimensionlessRuntimeAlarms(t *testing.T, stack awscdk.Stack) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	found := assertions.Template_FromStack(stack, nil).
		FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil)
	if found == nil {
		return out
	}
	for _, raw := range *found {
		props := (*raw)["Properties"].(map[string]any)
		namespace, _ := props["Namespace"].(string)
		metric, _ := props["MetricName"].(string)
		// Anything not published by an AWS service is a runtime series. Matching the
		// runtime namespace by name instead would let a deployment that renames it
		// silently empty this check rather than fail it.
		if metric == "" || namespace == "" ||
			strings.HasPrefix(namespace, "AWS/") || strings.HasPrefix(namespace, "ECS/") {
			continue
		}
		if _, dimensioned := props["Dimensions"]; dimensioned {
			continue
		}
		out[metric] = true
	}
	return out
}

// TestAlarmInventory_EveryDimensionlessAlarmHasARollupCopy closes the one failure
// mode a synthesized alarm cannot report about itself. The runtime emits most
// metrics with a route, session or partition dimension; a zero-dimension alarm
// never matches a dimensioned series, so an alarm on a metric with no rollup copy
// sits at INSUFFICIENT_DATA forever. It does not fail — it simply never fires,
// which is indistinguishable from health on every dashboard.
func TestAlarmInventory_EveryDimensionlessAlarmHasARollupCopy(t *testing.T) {
	haStack, rollupStack := everyShapeStack(t)
	rollups := documentedRollupMetrics(t)

	alarmed := dimensionlessRuntimeAlarms(t, haStack)
	for metric := range dimensionlessRuntimeAlarms(t, rollupStack) {
		alarmed[metric] = true
	}

	var unmatchable []string
	for metric := range alarmed {
		// The failover probe publishes this one itself, with no dimensions and no
		// runtime exporter involved, so it needs no rollup copy.
		if metric == gobridgealarms.FailureToFullMetricName {
			continue
		}
		if !rollups[metric] {
			unmatchable = append(unmatchable, metric)
		}
	}
	sort.Strings(unmatchable)
	if len(unmatchable) > 0 {
		t.Fatalf("alarms read these runtime metrics with no dimensions, but %s does not list them as "+
			"rolled up, so nothing they can match is ever published: %s",
			alarmInventoryDoc, strings.Join(unmatchable, ", "))
	}
}

// documentedMissingData maps each inventory row to the TreatMissingData
// behaviour the page attributes to it.
func documentedMissingData(t *testing.T) map[string]string {
	t.Helper()
	body, err := os.ReadFile(alarmInventoryDoc)
	if err != nil {
		t.Fatalf("the alarm inventory page must exist: %v", err)
	}

	out := map[string]string{}
	inSection := false
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, alarmInventoryHeading) {
			inSection = true
			continue
		}
		if !inSection {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			break
		}
		if match := alarmMissingDataCell.FindStringSubmatch(line); match != nil {
			out[match[1]] = strings.ReplaceAll(match[2], " ", "")
		}
	}
	return out
}

// synthesizedMissingData maps each alarm to its TreatMissingData setting.
func synthesizedMissingData(t *testing.T, stack awscdk.Stack, prefix string) map[string]string {
	t.Helper()
	out := map[string]string{}
	found := assertions.Template_FromStack(stack, nil).
		FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil)
	if found == nil {
		return out
	}
	for logicalID, raw := range *found {
		id := cdkLogicalIDHash.ReplaceAllString(logicalID, "")
		id = cdkSiblingSuffix.ReplaceAllString(strings.TrimPrefix(id, prefix), "")
		treat, _ := (*raw)["Properties"].(map[string]any)["TreatMissingData"].(string)
		out[id] = treat
	}
	return out
}

// TestAlarmInventory_PublishesTheRealMissingDataBehaviour keeps the one column an
// operator reads to decide whether silence means health. "breaching" makes an
// absent metric an outage — it is what catches a dead process — and
// "notBreaching" makes it health. Getting the two backwards on the page is worse
// than omitting the column: it tells an operator a silent alarm is covering them.
func TestAlarmInventory_PublishesTheRealMissingDataBehaviour(t *testing.T) {
	haStack, rollupStack := everyShapeStack(t)

	synthesized := synthesizedMissingData(t, haStack, "Alarms")
	for id, treat := range synthesizedMissingData(t, rollupStack, "Alarms") {
		synthesized[id] = treat
	}
	documented := documentedMissingData(t)

	var wrong []string
	for id, treat := range synthesized {
		got, ok := documented[id]
		if !ok {
			wrong = append(wrong, id+": the page states no missing-data behaviour")
			continue
		}
		if !strings.EqualFold(got, treat) {
			wrong = append(wrong, id+": page says "+got+", the template says "+treat)
		}
	}
	sort.Strings(wrong)
	if len(wrong) > 0 {
		t.Fatalf("%s misreports how these alarms treat missing data: %s",
			alarmInventoryDoc, strings.Join(wrong, "; "))
	}
}
