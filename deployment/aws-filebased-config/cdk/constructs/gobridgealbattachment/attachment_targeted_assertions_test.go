//go:build !race

package gobridgealbattachment_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/jsii-runtime-go"

	_ "github.com/mariotoffia/gobridge/adapters/http/transport"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
)

// Test_T20_Attachment_Single_PrioritiesContiguousFromBase: with 2 HTTP
// receivers in yaml the Single attachment emits 5 ListenerRules. Their
// priorities must form a contiguous block at BasePriority + step*0..N (step
// 10, default base 100): 100,110,120,130,140.
func Test_T20_Attachment_Single_PrioritiesContiguousFromBase(t *testing.T) {
	defer jsii.Close()
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, httpReceiverYAML))
	single := newSingle(t, stack, vpc, src)
	gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single: single, Listener: listener, Vpc: vpc, BridgeConfig: src,
	})
	tpl := assertions.Template_FromStack(stack, nil)

	tpl.ResourceCountIs(jsii.String("AWS::ElasticLoadBalancingV2::ListenerRule"), jsii.Number(5))
	prios := collectRulePriorities(tpl)
	sort.Float64s(prios)
	want := []float64{100, 110, 120, 130, 140}
	if len(prios) != len(want) {
		t.Fatalf("priority count = %d, want %d (got %v)", len(prios), len(want), prios)
	}
	for i, p := range prios {
		if p != want[i] {
			t.Fatalf("priority[%d] = %v, want %v (full %v)", i, p, want[i], prios)
		}
	}
}

// Test_T20_Attachment_TargetGroup_HealthCheckTunedDefaults pins the tuned TG
// defaults from the design doc lines 265-270: Interval=15s, Timeout=5s,
// HealthyThreshold=2, UnhealthyThreshold=2, plus protocol HTTP and matcher
// 200. The existing test exercises Interval/Timeout/Threshold but does NOT
// pin HealthCheckProtocol or the Matcher.
func Test_T20_Attachment_TargetGroup_HealthCheckTunedDefaults(t *testing.T) {
	defer jsii.Close()
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, baseYAML))
	single := newSingle(t, stack, vpc, src)
	gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single: single, Listener: listener, Vpc: vpc, BridgeConfig: src,
	})
	tpl := assertions.Template_FromStack(stack, nil)

	tpl.ResourceCountIs(jsii.String("AWS::ElasticLoadBalancingV2::TargetGroup"), jsii.Number(2))
	tgs := tpl.FindResources(jsii.String("AWS::ElasticLoadBalancingV2::TargetGroup"), nil)
	for id, raw := range *tgs {
		props := (*raw)["Properties"].(map[string]any)
		if got := props["HealthCheckPath"]; got != "/healthz" {
			t.Fatalf("TG %s HealthCheckPath = %v, want /healthz", id, got)
		}
		// HealthCheckProtocol and Matcher default to HTTP/200 implicitly when
		// the TG's traffic protocol is HTTP — CDK omits them from the CFn
		// template in that case. Assert only when explicitly emitted.
		if got, ok := props["HealthCheckProtocol"]; ok {
			if got != "HTTP" {
				t.Fatalf("TG %s HealthCheckProtocol = %v, want HTTP", id, got)
			}
		}
		if got := props["HealthCheckIntervalSeconds"]; got != 15.0 {
			t.Fatalf("TG %s Interval = %v, want 15", id, got)
		}
		if got := props["HealthCheckTimeoutSeconds"]; got != 5.0 {
			t.Fatalf("TG %s Timeout = %v, want 5", id, got)
		}
		if got := props["HealthyThresholdCount"]; got != 2.0 {
			t.Fatalf("TG %s HealthyThreshold = %v, want 2", id, got)
		}
		if got := props["UnhealthyThresholdCount"]; got != 2.0 {
			t.Fatalf("TG %s UnhealthyThreshold = %v, want 2", id, got)
		}
		if matcher, ok := props["Matcher"].(map[string]any); ok && matcher != nil {
			if got, _ := matcher["HttpCode"].(string); got != "200" {
				t.Fatalf("TG %s Matcher.HttpCode = %q, want 200", id, got)
			}
		}
		// Assert traffic protocol HTTP — this anchors the implicit defaults
		// above (when TG protocol is HTTP, health check defaults are HTTP/200).
		if got := props["Protocol"]; got != "HTTP" {
			t.Fatalf("TG %s Protocol = %v, want HTTP", id, got)
		}
	}
}

// Test_T20_Attachment_Cluster_TGsTargetCorrectServices pins the TG-to-Service
// wiring on the cluster scenario: the TG referenced by the Control service's
// LoadBalancers[0] must be the ControlTargetGroup, and same for Worker. The
// existing TestALBAttachment_Cluster_TGsTargetCorrectServices only counts
// LBs per service (1 each); this test asserts the ARN match goes to the
// correct TG.
func Test_T20_Attachment_Cluster_TGsTargetCorrectServices(t *testing.T) {
	defer jsii.Close()
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, baseYAML))
	cluster := newCluster(t, stack, vpc, src)
	att := gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Cluster: cluster, Listener: listener, Vpc: vpc, BridgeConfig: src,
	})
	tpl := assertions.Template_FromStack(stack, nil)

	// Resolve the construct's TG ARN attribute references back to a logical id.
	ctrlTGRef := refOfAttribute(awscdk.Stack_Of(stack), att.ControlTargetGroup().TargetGroupArn())
	wrkTGRef := refOfAttribute(awscdk.Stack_Of(stack), att.WorkerTargetGroup().TargetGroupArn())
	if ctrlTGRef == "" || wrkTGRef == "" {
		t.Fatalf("could not resolve TG logical ids: ctrl=%q wrk=%q", ctrlTGRef, wrkTGRef)
	}

	svcs := tpl.FindResources(jsii.String("AWS::ECS::Service"), nil)
	if len(*svcs) != 2 {
		t.Fatalf("want 2 services, got %d", len(*svcs))
	}
	matched := map[string]string{} // role -> tg ref
	for id, raw := range *svcs {
		props := (*raw)["Properties"].(map[string]any)
		lbs, _ := props["LoadBalancers"].([]any)
		if len(lbs) != 1 {
			t.Fatalf("service %s LoadBalancers count = %d, want 1", id, len(lbs))
		}
		lb := lbs[0].(map[string]any)
		tgArn, _ := lb["TargetGroupArn"].(map[string]any)
		ref, _ := tgArn["Ref"].(string)
		switch {
		case strings.Contains(id, "Control"):
			matched["Control"] = ref
		case strings.Contains(id, "Worker"):
			matched["Worker"] = ref
		default:
			t.Fatalf("unexpected service id %s", id)
		}
	}
	if matched["Control"] != ctrlTGRef {
		t.Fatalf("Control service TG ref = %q, want %q", matched["Control"], ctrlTGRef)
	}
	if matched["Worker"] != wrkTGRef {
		t.Fatalf("Worker service TG ref = %q, want %q", matched["Worker"], wrkTGRef)
	}
}

// refOfAttribute resolves a CDK token (e.g. tg.TargetGroupArn() returns a
// Ref to the TG logical id) and extracts the Ref string. Returns "" if the
// resolved form is not a {"Ref": "..."} intrinsic.
func refOfAttribute(stack awscdk.Stack, attr *string) string {
	v := stack.Resolve(attr)
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	r, _ := m["Ref"].(string)
	return r
}
