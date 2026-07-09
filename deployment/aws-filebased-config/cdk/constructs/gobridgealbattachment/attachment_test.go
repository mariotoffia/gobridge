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
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/jsii-runtime-go"

	// Register the http transport plugin so yaml parsing of
	// "transport: http" succeeds in tests that exercise receiver
	// path derivation.
	_ "github.com/mariotoffia/gobridge/adapters/http/transport"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgecluster"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/ssmexports"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
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
// file-based runtime IGNORES this block (lib/bootstrap.checkIgnoredHTTPBlock),
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
		Image:        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap:    bootstrap(),
		BridgeConfig: src,
	})
}

func newCluster(t *testing.T, stack awscdk.Stack, vpc awsec2.IVpc, src source.Source) *gobridgecluster.GoBridgeCluster {
	t.Helper()
	return gobridgecluster.NewGoBridgeCluster(stack, jsii.String("Cluster"), &gobridgecluster.ClusterProps{
		Vpc:          vpc,
		Image:        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
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
	defer jsii.Close()
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
	defer jsii.Close()
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
	defer jsii.Close()
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
	defer jsii.Close()
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

// TestALBAttachment_PortsPerConcern is the core B1 regression guard:
// each concern's target group must sit on the container port the
// process actually serves — admin (8080), monitor (8081), transport
// (8082) — so ALB traffic and health checks reach the right listener.
func TestALBAttachment_PortsPerConcern(t *testing.T) {
	defer jsii.Close()
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, httpReceiverYAML))
	single := newSingle(t, stack, vpc, src)
	att := gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single: single, Listener: listener, Vpc: vpc, BridgeConfig: src,
	})
	tpl := assertions.Template_FromStack(stack, nil)
	tgs := *tpl.FindResources(jsii.String("AWS::ElasticLoadBalancingV2::TargetGroup"), nil)
	st := awscdk.Stack_Of(stack)
	portOf := func(tg elbv2.ApplicationTargetGroup) float64 {
		ref := refOfAttribute(st, tg.TargetGroupArn())
		raw, ok := tgs[ref]
		if !ok {
			t.Fatalf("target group %q not found in template", ref)
		}
		props := (*raw)["Properties"].(map[string]any)
		p, _ := props["Port"].(float64)
		return p
	}
	if got := portOf(att.ControlTargetGroup()); got != 8080 {
		t.Fatalf("control TG Port = %v, want 8080 (admin)", got)
	}
	if got := portOf(att.MonitorTargetGroup()); got != 8081 {
		t.Fatalf("monitor TG Port = %v, want 8081", got)
	}
	if got := portOf(att.WorkerTargetGroup()); got != 8082 {
		t.Fatalf("worker (transport) TG Port = %v, want 8082 — receivers must route to the transport port, not admin", got)
	}
}

// TestALBAttachment_Ports_IgnoreHTTPOverride_MatchBootstrap is the
// c15-cdk-ports port-agreement guard at the synthesized-resource level:
// the bridge yaml sets an `http:` block (admin_addr :9090,
// monitor_addr :9091) that the file-based runtime IGNORES
// (lib/bootstrap.checkIgnoredHTTPBlock). The runtime binds only to the
// BootstrapConfig listen ports, so the emitted target-group ports AND
// the health-check port MUST match those bootstrap ports (8080 admin,
// 8081 monitor, 8082 transport), never the http: overrides. Otherwise
// the ALB would health-check and route to ports nothing listens on.
//
// Mutation: repoint DerivePortMappings back at cfg.HTTP → the control TG
// flips to 9090, the monitor TG + HealthCheckPort flip to 9091, and this
// test FAILs.
func TestALBAttachment_Ports_IgnoreHTTPOverride_MatchBootstrap(t *testing.T) {
	defer jsii.Close()
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, httpOverrideReceiverYAML))
	single := newSingle(t, stack, vpc, src)
	att := gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single: single, Listener: listener, Vpc: vpc, BridgeConfig: src,
	})
	tpl := assertions.Template_FromStack(stack, nil)
	tgs := *tpl.FindResources(jsii.String("AWS::ElasticLoadBalancingV2::TargetGroup"), nil)
	st := awscdk.Stack_Of(stack)
	propsOf := func(tg elbv2.ApplicationTargetGroup) map[string]any {
		ref := refOfAttribute(st, tg.TargetGroupArn())
		raw, ok := tgs[ref]
		if !ok {
			t.Fatalf("target group %q not found in template", ref)
		}
		return (*raw)["Properties"].(map[string]any)
	}

	// Traffic ports pinned to the bootstrap listen ports, NOT the
	// http: block's 9090/9091.
	cp, _ := propsOf(att.ControlTargetGroup())["Port"].(float64)
	if cp != 8080 {
		t.Fatalf("control TG Port = %v, want 8080 (bootstrap admin) — http.admin_addr :9090 must NOT sway it", cp)
	}
	mp, _ := propsOf(att.MonitorTargetGroup())["Port"].(float64)
	if mp != 8081 {
		t.Fatalf("monitor TG Port = %v, want 8081 (bootstrap monitor) — http.monitor_addr :9091 must NOT sway it", mp)
	}
	wp, _ := propsOf(att.WorkerTargetGroup())["Port"].(float64)
	if wp != 8082 {
		t.Fatalf("worker TG Port = %v, want 8082 (bootstrap transport)", wp)
	}

	// Every health check probes the monitor listen port. It must be the
	// bootstrap monitor port (8081), never http.monitor_addr (9091), or
	// the probes hit a port nothing listens on and deploys fail.
	for _, raw := range tgs {
		hcp := (*raw)["Properties"].(map[string]any)["HealthCheckPort"]
		if hcp != "8081" {
			t.Fatalf("HealthCheckPort = %v, want \"8081\" (bootstrap monitor) — must ignore http.monitor_addr :9091", hcp)
		}
	}
}

func TestALBAttachment_HealthCheckDefaults(t *testing.T) {
	defer jsii.Close()
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, baseYAML))
	single := newSingle(t, stack, vpc, src)
	gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single: single, Listener: listener, Vpc: vpc, BridgeConfig: src,
	})
	tpl := assertions.Template_FromStack(stack, nil)
	tgs := tpl.FindResources(jsii.String("AWS::ElasticLoadBalancingV2::TargetGroup"), nil)
	for _, raw := range *tgs {
		props := (*raw)["Properties"].(map[string]any)
		if props["HealthCheckPath"] != "/api/v1/monitor/live" {
			t.Fatalf("HealthCheckPath = %v, want /api/v1/monitor/live", props["HealthCheckPath"])
		}
		// Every TG probes the monitor port (8081) via the health-check
		// port override, regardless of its own traffic port.
		if props["HealthCheckPort"] != "8081" {
			t.Fatalf("HealthCheckPort = %v, want \"8081\"", props["HealthCheckPort"])
		}
		if props["HealthCheckIntervalSeconds"] != 15.0 {
			t.Fatalf("HealthCheckIntervalSeconds = %v, want 15", props["HealthCheckIntervalSeconds"])
		}
		if props["HealthCheckTimeoutSeconds"] != 5.0 {
			t.Fatalf("HealthCheckTimeoutSeconds = %v, want 5", props["HealthCheckTimeoutSeconds"])
		}
		if props["HealthyThresholdCount"] != 2.0 {
			t.Fatalf("HealthyThresholdCount = %v, want 2", props["HealthyThresholdCount"])
		}
		if props["UnhealthyThresholdCount"] != 2.0 {
			t.Fatalf("UnhealthyThresholdCount = %v, want 2", props["UnhealthyThresholdCount"])
		}
	}
}

func TestALBAttachment_HealthCheckOverride(t *testing.T) {
	defer jsii.Close()
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, baseYAML))
	single := newSingle(t, stack, vpc, src)
	gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single: single, Listener: listener, Vpc: vpc, BridgeConfig: src,
		HealthCheck: &gobridgealbattachment.HealthCheckProps{Path: "/custom/health"},
	})
	tpl := assertions.Template_FromStack(stack, nil)
	tgs := tpl.FindResources(jsii.String("AWS::ElasticLoadBalancingV2::TargetGroup"), nil)
	for _, raw := range *tgs {
		props := (*raw)["Properties"].(map[string]any)
		if props["HealthCheckPath"] != "/custom/health" {
			t.Fatalf("HealthCheckPath = %v, want /custom/health", props["HealthCheckPath"])
		}
		// A custom Path override is honored, but the health-check Port
		// stays pinned to the monitor port — the probes live only there.
		if props["HealthCheckPort"] != "8081" {
			t.Fatalf("HealthCheckPort = %v, want \"8081\"", props["HealthCheckPort"])
		}
	}
}

func TestALBAttachment_NegativeBasePriority_Panics(t *testing.T) {
	defer jsii.Close()
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
	defer jsii.Close()
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
	defer jsii.Close()
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
	defer jsii.Close()
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
	defer jsii.Close()
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
	defer jsii.Close()
	_, stack, _, _ := newApp(t)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil props")
		}
	}()
	gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), nil)
}

// --- T15: accessors + outputs ---

func newAttachment(t *testing.T, prio int) (awscdk.Stack, *gobridgealbattachment.GoBridgeALBAttachment) {
	t.Helper()
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, baseYAML))
	single := newSingle(t, stack, vpc, src)
	att := gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single: single, Listener: listener, Vpc: vpc, BridgeConfig: src,
		BasePriority: prio,
	})
	return stack, att
}

func TestALBAttachment_URLAccessors_NonNil(t *testing.T) {
	defer jsii.Close()
	_, att := newAttachment(t, 0)
	if att.PublicDnsName() == nil {
		t.Fatal("PublicDnsName nil")
	}
	if att.AdminURL() == nil {
		t.Fatal("AdminURL nil")
	}
	if att.HealthzURL() == nil {
		t.Fatal("HealthzURL nil")
	}
}

func resolveOutput(t *testing.T, stack awscdk.Stack, value *string) string {
	t.Helper()
	return *awscdk.Stack_Of(stack).Resolve(value).(*string)
}

func TestALBAttachment_AdminURL_HasPathSuffix(t *testing.T) {
	defer jsii.Close()
	stack, att := newAttachment(t, 0)
	resolved := awscdk.Stack_Of(stack).Resolve(att.AdminURL())
	// Resolution returns a Fn::Join intrinsic; serialize and check
	// the literal path suffix is present.
	s := fmt.Sprintf("%v", resolved)
	if !strings.Contains(s, "https://") {
		t.Fatalf("AdminURL resolved form lacks https://: %s", s)
	}
	if !strings.Contains(s, "/api/v1/") {
		t.Fatalf("AdminURL resolved form lacks /api/v1/ suffix: %s", s)
	}
	_ = resolveOutput // keep helper in scope for future use
}

func TestALBAttachment_HealthzURL_HasPathSuffix(t *testing.T) {
	defer jsii.Close()
	stack, att := newAttachment(t, 0)
	resolved := awscdk.Stack_Of(stack).Resolve(att.HealthzURL())
	s := fmt.Sprintf("%v", resolved)
	if !strings.Contains(s, "/api/v1/monitor/health") {
		t.Fatalf("HealthzURL resolved form lacks /api/v1/monitor/health suffix: %s", s)
	}
}

func outputNames(tpl assertions.Template) map[string]bool {
	got := tpl.FindOutputs(jsii.String("*"), nil)
	out := map[string]bool{}
	if got == nil {
		return out
	}
	for name := range *got {
		out[name] = true
	}
	return out
}

func TestALBAttachment_WithCfnOutputs_Prefixed(t *testing.T) {
	defer jsii.Close()
	stack, att := newAttachment(t, 0)
	att.WithCfnOutputs("OrdersBridge")
	tpl := assertions.Template_FromStack(stack, nil)
	names := outputNames(tpl)
	for _, want := range []string{"OrdersBridgeAdminURL", "OrdersBridgeHealthzURL"} {
		if !names[want] {
			t.Fatalf("missing output %q (got %v)", want, names)
		}
	}
}

func TestALBAttachment_WithCfnOutputs_EmptyPrefix(t *testing.T) {
	defer jsii.Close()
	stack, att := newAttachment(t, 0)
	att.WithCfnOutputs("")
	tpl := assertions.Template_FromStack(stack, nil)
	names := outputNames(tpl)
	for _, want := range []string{"AdminURL", "HealthzURL"} {
		if !names[want] {
			t.Fatalf("missing output %q (got %v)", want, names)
		}
	}
}

func ssmParameterNames(tpl assertions.Template) []string {
	res := tpl.FindResources(jsii.String("AWS::SSM::Parameter"), nil)
	out := []string{}
	if res == nil {
		return out
	}
	for _, raw := range *res {
		props := (*raw)["Properties"].(map[string]any)
		if n, ok := props["Name"].(string); ok {
			out = append(out, n)
		}
	}
	return out
}

func TestALBAttachment_WithSSMExports_DefaultSet(t *testing.T) {
	defer jsii.Close()
	stack, att := newAttachment(t, 0)
	att.WithSSMExports("/gobridge/prod/test")
	tpl := assertions.Template_FromStack(stack, nil)
	tpl.ResourceCountIs(jsii.String("AWS::SSM::Parameter"), jsii.Number(3))
	names := ssmParameterNames(tpl)
	want := map[string]bool{
		"/gobridge/prod/test/admin-url":        true,
		"/gobridge/prod/test/healthz-url":      true,
		"/gobridge/prod/test/manifest-version": true,
	}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("unexpected SSM Name %q (allowed %v)", n, want)
		}
		delete(want, n)
	}
	if len(want) != 0 {
		t.Fatalf("missing SSM Names: %v", want)
	}
}

func TestALBAttachment_WithSSMExports_IncludeARNs(t *testing.T) {
	defer jsii.Close()
	stack, att := newAttachment(t, 0)
	att.WithSSMExports("/gobridge/prod/test", ssmexports.IncludeARNs())
	tpl := assertions.Template_FromStack(stack, nil)
	tpl.ResourceCountIs(jsii.String("AWS::SSM::Parameter"), jsii.Number(6))
	names := ssmParameterNames(tpl)
	want := map[string]bool{
		"/gobridge/prod/test/admin-url":        true,
		"/gobridge/prod/test/healthz-url":      true,
		"/gobridge/prod/test/manifest-version": true,
		"/gobridge/prod/test/alb-arn":          true,
		"/gobridge/prod/test/cluster-arn":      true,
		"/gobridge/prod/test/efs-id":           true,
	}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("unexpected SSM Name %q (allowed %v)", n, want)
		}
		delete(want, n)
	}
	if len(want) != 0 {
		t.Fatalf("missing SSM Names: %v", want)
	}
}

func TestALBAttachment_WithSSMExports_ManifestVersionMatchesConst(t *testing.T) {
	defer jsii.Close()
	stack, att := newAttachment(t, 0)
	att.WithSSMExports("/gobridge/prod/test")
	tpl := assertions.Template_FromStack(stack, nil)
	res := tpl.FindResources(jsii.String("AWS::SSM::Parameter"), nil)
	for _, raw := range *res {
		props := (*raw)["Properties"].(map[string]any)
		if props["Name"] != "/gobridge/prod/test/manifest-version" {
			continue
		}
		if got := props["Value"]; got != gobridgealbattachment.ManifestVersion {
			t.Fatalf("manifest-version Value = %v, want %s", got, gobridgealbattachment.ManifestVersion)
		}
		return
	}
	t.Fatal("manifest-version parameter not found")
}

func TestALBAttachment_WithSSMExports_EmptyPrefix_Panics(t *testing.T) {
	defer jsii.Close()
	_, att := newAttachment(t, 0)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on empty prefix")
		}
		if !strings.Contains(fmt.Sprintf("%v", r), "must not be empty") {
			t.Fatalf("panic message = %v", r)
		}
	}()
	att.WithSSMExports("")
}

func TestALBAttachment_WithSSMExports_NoLeadingSlash_Panics(t *testing.T) {
	defer jsii.Close()
	_, att := newAttachment(t, 0)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on missing leading slash")
		}
		if !strings.Contains(fmt.Sprintf("%v", r), "must start with '/'") {
			t.Fatalf("panic message = %v", r)
		}
	}()
	att.WithSSMExports("no-leading-slash")
}
