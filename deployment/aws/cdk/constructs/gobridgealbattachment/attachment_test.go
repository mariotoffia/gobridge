//go:build !race

package gobridgealbattachment_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/internal/imgsource"

	// Register the http transport plugin so yaml parsing of
	// "transport: http" succeeds in tests that exercise receiver
	// path derivation.
	_ "github.com/mariotoffia/gobridge/adapters/http/transport"

	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgealbattachment"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgecluster"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/constructs/gobridgesingle"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws/infra"
)

const baseYAML = `
bridge:
  id: test-bridge
`

const httpReceiverYAML = `
bridge:
  id: test-bridge
receivers:
  - id: webhook
    transport: http
    options:
      path: /hooks/webhook
  - id: events
    transport: http
`

// httpOverrideReceiverYAML sets a bridge-yaml `http:` block that
// overrides admin/monitor addresses to non-default ports. The
// AWS runtime IGNORES this block (lib/bootstrap.checkIgnoredHTTPBlock),
// so the synthesized target-group + health-check ports MUST stay on the
// BootstrapConfig listen ports (8080/8081/8082), never the http: values.
const httpOverrideReceiverYAML = `
bridge:
  id: test-bridge
http:
  admin_addr: ":9090"
  monitor_addr: ":9091"
receivers:
  - id: webhook
    transport: http
    options:
      path: /hooks/webhook
`

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return p
}

func bootstrap() infra.BootstrapConfig {
	return infra.BootstrapConfig{
		BridgeID:         "bridge-1",
		ConfigFilePath:   "/var/lib/gobridge/bridge.yaml",
		AdminAPIKeyParam: "/test/admin",
	}
}

func newApp(t *testing.T) (awscdk.App, awscdk.Stack, awsec2.IVpc, elbv2.IApplicationListener) {
	t.Helper()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("S"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	alb := elbv2.NewApplicationLoadBalancer(stack, jsii.String("ALB"), &elbv2.ApplicationLoadBalancerProps{
		Vpc: vpc, InternetFacing: jsii.Bool(true),
	})
	listener := alb.AddListener(jsii.String("L"), &elbv2.BaseApplicationListenerProps{
		Port:          jsii.Number(80),
		Protocol:      elbv2.ApplicationProtocol_HTTP,
		Open:          jsii.Bool(false),
		DefaultAction: elbv2.ListenerAction_FixedResponse(jsii.Number(404), nil),
	})
	return app, stack, vpc, listener
}

func newSingle(t *testing.T, stack awscdk.Stack, vpc awsec2.IVpc, src source.Source) *gobridgesingle.GoBridgeSingle {
	t.Helper()
	return gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Single"), &gobridgesingle.SingleProps{
		Vpc:          vpc,
		Image:        imgsource.NewRegistry("gobridge@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Bootstrap:    bootstrap(),
		BridgeConfig: src,
	})
}

func newCluster(t *testing.T, stack awscdk.Stack, vpc awsec2.IVpc, src source.Source) *gobridgecluster.GoBridgeCluster {
	t.Helper()
	return gobridgecluster.NewGoBridgeCluster(stack, jsii.String("Cluster"), &gobridgecluster.ClusterProps{
		Vpc:          vpc,
		Image:        imgsource.NewRegistry("gobridge@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Bootstrap:    bootstrap(),
		BridgeConfig: src,
	})
}

func collectRulePriorities(tpl assertions.Template) []float64 {
	rules := tpl.FindResources(jsii.String("AWS::ElasticLoadBalancingV2::ListenerRule"), nil)
	out := []float64{}
	if rules == nil {
		return out
	}
	for _, raw := range *rules {
		props := (*raw)["Properties"].(map[string]any)
		if p, ok := props["Priority"].(float64); ok {
			out = append(out, p)
		}
	}
	return out
}

func collectPathPatterns(tpl assertions.Template) map[float64][]string {
	out := map[float64][]string{}
	rules := tpl.FindResources(jsii.String("AWS::ElasticLoadBalancingV2::ListenerRule"), nil)
	if rules == nil {
		return out
	}
	for _, raw := range *rules {
		props := (*raw)["Properties"].(map[string]any)
		prio, _ := props["Priority"].(float64)
		conds, _ := props["Conditions"].([]any)
		paths := []string{}
		for _, c := range conds {
			cm := c.(map[string]any)
			if cm["Field"] != "path-pattern" {
				continue
			}
			vals, _ := cm["Values"].([]any)
			for _, v := range vals {
				if s, ok := v.(string); ok {
					paths = append(paths, s)
				}
			}
			if pcm, ok := cm["PathPatternConfig"].(map[string]any); ok {
				vals2, _ := pcm["Values"].([]any)
				for _, v := range vals2 {
					if s, ok := v.(string); ok {
						paths = append(paths, s)
					}
				}
			}
		}
		out[prio] = paths
	}
	return out
}

func TestALBAttachment_Single_Synth(t *testing.T) {
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, httpReceiverYAML))
	single := newSingle(t, stack, vpc, src)

	att := gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single:       single,
		Listener:     listener,
		Vpc:          vpc,
		BridgeConfig: src,
	})
	if att.ControlTargetGroup() == nil || att.MonitorTargetGroup() == nil || att.WorkerTargetGroup() == nil {
		t.Fatal("target groups nil")
	}
	if att.Listener() == nil {
		t.Fatal("listener nil")
	}
	if got := att.BasePriority(); got != 100 {
		t.Fatalf("BasePriority=%d, want 100", got)
	}

	tpl := assertions.Template_FromStack(stack, nil)
	tpl.ResourceCountIs(jsii.String("AWS::ElasticLoadBalancingV2::TargetGroup"), jsii.Number(3))
	// 3 fixed rules (monitor, admin status, admin api) + 2 receivers
	tpl.ResourceCountIs(jsii.String("AWS::ElasticLoadBalancingV2::ListenerRule"), jsii.Number(5))

	prios := collectRulePriorities(tpl)
	wantPrios := map[float64]bool{100: true, 110: true, 120: true, 130: true, 140: true}
	for _, p := range prios {
		if !wantPrios[p] {
			t.Fatalf("unexpected rule priority %v (want one of %v)", p, wantPrios)
		}
		delete(wantPrios, p)
	}
	if len(wantPrios) != 0 {
		t.Fatalf("missing priorities: %v", wantPrios)
	}

	paths := collectPathPatterns(tpl)
	mustHavePath := func(prio float64, want string) {
		ps := paths[prio]
		for _, p := range ps {
			if p == want {
				return
			}
		}
		t.Fatalf("priority %v missing path %q (got %v)", prio, want, ps)
	}
	mustHavePath(100, "/api/v1/monitor/*")
	mustHavePath(110, "/api/v1/status*")
	mustHavePath(120, "/api/v1/*")
	mustHavePath(130, "/hooks/webhook")
	mustHavePath(140, "/transport/http/receivers/events/messages")
}

func TestALBAttachment_Single_AllTGsTargetSingleService(t *testing.T) {
	_, stack, vpc, listener := newApp(t)
	// httpReceiverYAML declares an HTTP receiver, so all three target
	// groups (control, monitor, transport) are emitted.
	src := source.NewAsset(writeYAML(t, httpReceiverYAML))
	single := newSingle(t, stack, vpc, src)
	gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single: single, Listener: listener, Vpc: vpc, BridgeConfig: src,
	})
	tpl := assertions.Template_FromStack(stack, nil)

	// Single facade ⇒ exactly 1 ECS service. All three TGs (control,
	// monitor, worker) reference it via the service's
	// LoadBalancers[].TargetGroupArn — so the service resource must
	// have 3 LoadBalancers entries (one per TG), confirming all
	// attached.
	svcs := tpl.FindResources(jsii.String("AWS::ECS::Service"), nil)
	if len(*svcs) != 1 {
		t.Fatalf("ECS::Service count = %d, want 1", len(*svcs))
	}
	for _, raw := range *svcs {
		props := (*raw)["Properties"].(map[string]any)
		lbs, _ := props["LoadBalancers"].([]any)
		if len(lbs) != 3 {
			t.Fatalf("Single service LoadBalancers count = %d, want 3 (one per TG)", len(lbs))
		}
	}
}

func TestALBAttachment_Cluster_TGsTargetCorrectServices(t *testing.T) {
	_, stack, vpc, listener := newApp(t)
	// httpReceiverYAML declares an HTTP receiver, so the worker service
	// gets its own transport target group in addition to the shared
	// monitor TG (2 LoadBalancers each, symmetric with control).
	src := source.NewAsset(writeYAML(t, httpReceiverYAML))
	cluster := newCluster(t, stack, vpc, src)
	gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Cluster: cluster, Listener: listener, Vpc: vpc, BridgeConfig: src,
	})
	tpl := assertions.Template_FromStack(stack, nil)

	svcs := tpl.FindResources(jsii.String("AWS::ECS::Service"), nil)
	if len(*svcs) != 2 {
		t.Fatalf("ECS::Service count = %d, want 2", len(*svcs))
	}
	// Each cluster service attaches to its own TG (control→ControlTG,
	// worker→WorkerTG) plus the shared MonitorTG that every service
	// joins — so exactly 2 LoadBalancers entries per service.
	for _, raw := range *svcs {
		props := (*raw)["Properties"].(map[string]any)
		lbs, _ := props["LoadBalancers"].([]any)
		if len(lbs) != 2 {
			t.Fatalf("each cluster service should attach to exactly 2 TGs (own + monitor), got %d", len(lbs))
		}
	}
	tpl.ResourceCountIs(jsii.String("AWS::ElasticLoadBalancingV2::TargetGroup"), jsii.Number(3))
}

// TestALBAttachment_NoReceiver_WorkerFallsBackToMonitor pins the
// behaviour when the yaml declares no HTTP receiver: no transport
// target group is created (only control + monitor), and
// WorkerTargetGroup falls back to the monitor target group so
// downstream consumers (e.g. the alarms construct) still get an
// LB-attached target group. This is the regression guard for the
// alarms `TargetGroup needs to be attached to a LoadBalancer` panic.
func TestALBAttachment_NoReceiver_WorkerFallsBackToMonitor(t *testing.T) {
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, baseYAML))
	single := newSingle(t, stack, vpc, src)
	att := gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single: single, Listener: listener, Vpc: vpc, BridgeConfig: src,
	})

	// Only control + monitor target groups exist.
	tpl := assertions.Template_FromStack(stack, nil)
	tpl.ResourceCountIs(jsii.String("AWS::ElasticLoadBalancingV2::TargetGroup"), jsii.Number(2))

	// WorkerTargetGroup resolves to the monitor target group (same
	// underlying resource), which carries the "/api/v1/monitor/*" rule
	// and is therefore attached to the listener.
	st := awscdk.Stack_Of(stack)
	wrkRef := refOfAttribute(st, att.WorkerTargetGroup().TargetGroupArn())
	monRef := refOfAttribute(st, att.MonitorTargetGroup().TargetGroupArn())
	if wrkRef == "" || wrkRef != monRef {
		t.Fatalf("WorkerTargetGroup ref = %q, want it to equal MonitorTargetGroup ref %q", wrkRef, monRef)
	}
}

// TestALBAttachment_PortsPerConcern is the core regression guard:
// each concern's target group must sit on the container port the
// process actually serves — admin (8080), monitor (8081), transport
// (8082) — so ALB traffic and health checks reach the right listener.
func TestALBAttachment_NegativeBasePriority_Panics(t *testing.T) {
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, baseYAML))
	single := newSingle(t, stack, vpc, src)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on negative BasePriority")
		}
	}()
	gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single: single, Listener: listener, Vpc: vpc, BridgeConfig: src,
		BasePriority: -1,
	})
}

func TestALBAttachment_BothFacadesNil_Panics(t *testing.T) {
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, baseYAML))
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when both Single and Cluster nil")
		}
		if !strings.Contains(fmt.Sprintf("%v", r), "exactly one") {
			t.Fatalf("panic message = %v, want 'exactly one' guard", r)
		}
	}()
	gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Listener: listener, Vpc: vpc, BridgeConfig: src,
	})
}

func TestALBAttachment_BothFacadesSet_Panics(t *testing.T) {
	app1 := awscdk.NewApp(nil)
	stack1 := awscdk.NewStack(app1, jsii.String("S1"), nil)
	vpc1 := awsec2.NewVpc(stack1, jsii.String("Vpc"), nil)
	src1 := source.NewAsset(writeYAML(t, baseYAML))
	single := newSingle(t, stack1, vpc1, src1)

	app2 := awscdk.NewApp(nil)
	stack2 := awscdk.NewStack(app2, jsii.String("S2"), nil)
	vpc2 := awsec2.NewVpc(stack2, jsii.String("Vpc"), nil)
	src2 := source.NewAsset(writeYAML(t, baseYAML))
	cluster := newCluster(t, stack2, vpc2, src2)

	_, stack3, vpc3, listener := newApp(t)
	src3 := source.NewAsset(writeYAML(t, baseYAML))
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when both Single and Cluster set")
		}
	}()
	gobridgealbattachment.NewGoBridgeALBAttachment(stack3, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single: single, Cluster: cluster, Listener: listener, Vpc: vpc3, BridgeConfig: src3,
	})
}

func TestALBAttachment_PriorityCollision_Panics(t *testing.T) {
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, baseYAML))
	single := newSingle(t, stack, vpc, src)

	// Plant a consumer rule on the listener at base+25, inside the
	// reserved [100..199] window.
	dummyTG := elbv2.NewApplicationTargetGroup(stack, jsii.String("DummyTG"), &elbv2.ApplicationTargetGroupProps{
		Vpc: vpc, Port: jsii.Number(8080), Protocol: elbv2.ApplicationProtocol_HTTP, TargetType: elbv2.TargetType_IP,
	})
	elbv2.NewApplicationListenerRule(stack, jsii.String("ConsumerRule"), &elbv2.ApplicationListenerRuleProps{
		Listener: listener,
		Priority: jsii.Number(125),
		Conditions: &[]elbv2.ListenerCondition{
			elbv2.ListenerCondition_PathPatterns(&[]*string{jsii.String("/consumer/*")}),
		},
		TargetGroups: &[]elbv2.IApplicationTargetGroup{dummyTG},
	})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on priority collision")
		}
		want := "ALB BasePriority 100 reserves [100..199]; consumer rule already uses 100+25"
		if fmt.Sprintf("%v", r) != want {
			t.Fatalf("collision panic message:\n got: %v\nwant: %s", r, want)
		}
	}()
	gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single: single, Listener: listener, Vpc: vpc, BridgeConfig: src,
	})
}

func TestALBAttachment_NoCollisionOutsideReservedRange(t *testing.T) {
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, baseYAML))
	single := newSingle(t, stack, vpc, src)

	dummyTG := elbv2.NewApplicationTargetGroup(stack, jsii.String("DummyTG"), &elbv2.ApplicationTargetGroupProps{
		Vpc: vpc, Port: jsii.Number(8080), Protocol: elbv2.ApplicationProtocol_HTTP, TargetType: elbv2.TargetType_IP,
	})
	elbv2.NewApplicationListenerRule(stack, jsii.String("Outside"), &elbv2.ApplicationListenerRuleProps{
		Listener: listener,
		Priority: jsii.Number(50),
		Conditions: &[]elbv2.ListenerCondition{
			elbv2.ListenerCondition_PathPatterns(&[]*string{jsii.String("/other/*")}),
		},
		TargetGroups: &[]elbv2.IApplicationTargetGroup{dummyTG},
	})

	gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single: single, Listener: listener, Vpc: vpc, BridgeConfig: src,
	})
	// No panic ⇒ pass.
}

func TestALBAttachment_NilProps_Panics(t *testing.T) {
	_, stack, _, _ := newApp(t)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil props")
		}
	}()
	gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), nil)
}

// --- accessors + outputs ---
