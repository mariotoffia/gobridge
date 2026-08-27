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
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, baseYAML))
	single := newSingle(t, stack, vpc, src)
	gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single: single, Listener: listener, Vpc: vpc, BridgeConfig: src,
	})
	tpl := assertions.Template_FromStack(stack, nil)

	// baseYAML declares no HTTP receiver, so only the control + monitor
	// target groups are emitted.
	tpl.ResourceCountIs(jsii.String("AWS::ElasticLoadBalancingV2::TargetGroup"), jsii.Number(2))
	tgs := tpl.FindResources(jsii.String("AWS::ElasticLoadBalancingV2::TargetGroup"), nil)
	for id, raw := range *tgs {
		props := (*raw)["Properties"].(map[string]any)
		if got := props["HealthCheckPath"]; got != "/api/v1/monitor/live" {
			t.Fatalf("TG %s HealthCheckPath = %v, want /api/v1/monitor/live", id, got)
		}
		// Every TG probes the monitor port (8081) via the health-check
		// port override, regardless of its own traffic port.
		if got := props["HealthCheckPort"]; got != "8081" {
			t.Fatalf("TG %s HealthCheckPort = %v, want \"8081\"", id, got)
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
// wiring on the cluster scenario. Each service attaches to exactly two target
// groups: its role-specific TG (Control→ControlTargetGroup,
// Worker→WorkerTargetGroup) plus the shared MonitorTargetGroup that every
// service joins so the monitor-port health checks are reachable. This test
// asserts the ARN references resolve to those exact TGs — the sibling
// TestALBAttachment_Cluster_TGsTargetCorrectServices only counts LBs.
func Test_T20_Attachment_Cluster_TGsTargetCorrectServices(t *testing.T) {
	_, stack, vpc, listener := newApp(t)
	// httpReceiverYAML emits the transport target group so the worker
	// service attaches to its own (transport) TG plus the shared monitor
	// TG — symmetric 2-LB wiring with the control service.
	src := source.NewAsset(writeYAML(t, httpReceiverYAML))
	cluster := newCluster(t, stack, vpc, src)
	att := gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Cluster: cluster, Listener: listener, Vpc: vpc, BridgeConfig: src,
	})
	tpl := assertions.Template_FromStack(stack, nil)

	// Resolve the construct's TG ARN attribute references back to logical ids.
	ctrlTGRef := refOfAttribute(awscdk.Stack_Of(stack), att.ControlTargetGroup().TargetGroupArn())
	monTGRef := refOfAttribute(awscdk.Stack_Of(stack), att.MonitorTargetGroup().TargetGroupArn())
	wrkTGRef := refOfAttribute(awscdk.Stack_Of(stack), att.WorkerTargetGroup().TargetGroupArn())
	if ctrlTGRef == "" || monTGRef == "" || wrkTGRef == "" {
		t.Fatalf("could not resolve TG logical ids: ctrl=%q mon=%q wrk=%q", ctrlTGRef, monTGRef, wrkTGRef)
	}

	has := func(list []string, v string) bool {
		for _, x := range list {
			if x == v {
				return true
			}
		}
		return false
	}

	svcs := tpl.FindResources(jsii.String("AWS::ECS::Service"), nil)
	if len(*svcs) != 2 {
		t.Fatalf("want 2 services, got %d", len(*svcs))
	}
	roleTG := map[string]string{} // role -> role-specific (non-monitor) tg ref
	for id, raw := range *svcs {
		props := (*raw)["Properties"].(map[string]any)
		lbs, _ := props["LoadBalancers"].([]any)
		if len(lbs) != 2 {
			t.Fatalf("service %s LoadBalancers count = %d, want 2 (own + monitor)", id, len(lbs))
		}
		refs := make([]string, 0, len(lbs))
		for _, l := range lbs {
			lb := l.(map[string]any)
			tgArn, _ := lb["TargetGroupArn"].(map[string]any)
			if ref, _ := tgArn["Ref"].(string); ref != "" {
				refs = append(refs, ref)
			}
		}
		// Every service must join the shared monitor TG.
		if !has(refs, monTGRef) {
			t.Fatalf("service %s LBs %v missing monitor TG %q", id, refs, monTGRef)
		}
		// The remaining ref is the role-specific TG.
		var own string
		for _, r := range refs {
			if r != monTGRef {
				own = r
			}
		}
		switch {
		case strings.Contains(id, "Control"):
			roleTG["Control"] = own
		case strings.Contains(id, "Worker"):
			roleTG["Worker"] = own
		default:
			t.Fatalf("unexpected service id %s", id)
		}
	}
	if roleTG["Control"] != ctrlTGRef {
		t.Fatalf("Control service role TG ref = %q, want %q", roleTG["Control"], ctrlTGRef)
	}
	if roleTG["Worker"] != wrkTGRef {
		t.Fatalf("Worker service role TG ref = %q, want %q", roleTG["Worker"], wrkTGRef)
	}
}

// Test_T20_Attachment_TransportTG_HealthChecksLiveness pins [HIGH-2] as fixed
// WITHOUT the ECS multi-TG recycle hazard: the transport (HTTP receiver)
// target group health-checks the LIVENESS probe (/api/v1/monitor/live), the
// SAME as control and monitor — NOT a broker-coupled readiness probe.
//
// Why liveness, not /ready?level=...: ECS replaces a task that is unhealthy in
// ANY attached target group, and the worker service is attached to BOTH this
// transport TG and the shared monitor TG. A readiness probe here would drive
// task replacement, so a broker-wide outage or a deliberate admin pause would
// flip every worker's /ready to 503 and recycle the whole fleet (a
// crash-recycle storm that amplifies a transient downstream outage). Traffic
// readiness is enforced at the request layer instead (the HTTP receiver
// returns 5xx and records the dedup key only on success, so producers retry
// with no message loss — adapters/http/transport/receiver.go:178,381,385).
//
// Mutation: flip the transport TG health-check default to /ready?level=full and
// this fails.
func Test_T20_Attachment_TransportTG_HealthChecksLiveness(t *testing.T) {
	_, stack, vpc, listener := newApp(t)
	// httpReceiverYAML declares HTTP receivers, so a dedicated transport
	// (worker) target group is emitted alongside control + monitor.
	src := source.NewAsset(writeYAML(t, httpReceiverYAML))
	single := newSingle(t, stack, vpc, src)
	att := gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single: single, Listener: listener, Vpc: vpc, BridgeConfig: src,
	})
	tpl := assertions.Template_FromStack(stack, nil)

	transportRef := refOfAttribute(awscdk.Stack_Of(stack), att.WorkerTargetGroup().TargetGroupArn())
	controlRef := refOfAttribute(awscdk.Stack_Of(stack), att.ControlTargetGroup().TargetGroupArn())
	monitorRef := refOfAttribute(awscdk.Stack_Of(stack), att.MonitorTargetGroup().TargetGroupArn())
	if transportRef == "" || controlRef == "" || monitorRef == "" {
		t.Fatalf("could not resolve TG logical ids: transport=%q control=%q monitor=%q", transportRef, controlRef, monitorRef)
	}
	// With HTTP receivers the worker TG is the dedicated transport TG, not the
	// monitor fallback — otherwise the transport-TG assertion would be vacuous.
	if transportRef == monitorRef {
		t.Fatalf("expected a dedicated transport TG, but WorkerTargetGroup fell back to the monitor TG")
	}

	tgs := tpl.FindResources(jsii.String("AWS::ElasticLoadBalancingV2::TargetGroup"), nil)
	pathOf := func(ref string) string {
		t.Helper()
		raw, ok := (*tgs)[ref]
		if !ok {
			t.Fatalf("TG %s not found in template", ref)
		}
		props := (*raw)["Properties"].(map[string]any)
		p, _ := props["HealthCheckPath"].(string)
		return p
	}

	// All three target groups — including the transport TG — probe /live. A
	// broker-coupled readiness probe here would recycle the worker fleet on a
	// broker outage/pause because of ECS multi-TG unhealthy semantics.
	if got := pathOf(transportRef); got != "/api/v1/monitor/live" {
		t.Fatalf("transport TG HealthCheckPath = %q, want /api/v1/monitor/live "+
			"(a broker-coupled /ready probe would recycle the worker fleet via ECS multi-TG semantics; "+
			"traffic readiness is enforced at the receiver instead)", got)
	}
	if got := pathOf(controlRef); got != "/api/v1/monitor/live" {
		t.Fatalf("control TG HealthCheckPath = %q, want /api/v1/monitor/live", got)
	}
	if got := pathOf(monitorRef); got != "/api/v1/monitor/live" {
		t.Fatalf("monitor TG HealthCheckPath = %q, want /api/v1/monitor/live", got)
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
