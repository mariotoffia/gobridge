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
		Port:                jsii.Number(80),
		Protocol:            elbv2.ApplicationProtocol_HTTP,
		Open:                jsii.Bool(false),
		DefaultAction:       elbv2.ListenerAction_FixedResponse(jsii.Number(404), nil),
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
	if att.ControlTargetGroup() == nil || att.WorkerTargetGroup() == nil {
		t.Fatal("target groups nil")
	}
	if att.Listener() == nil {
		t.Fatal("listener nil")
	}
	if got := att.BasePriority(); got != 100 {
		t.Fatalf("BasePriority=%d, want 100", got)
	}

	tpl := assertions.Template_FromStack(stack, nil)
	tpl.ResourceCountIs(jsii.String("AWS::ElasticLoadBalancingV2::TargetGroup"), jsii.Number(2))
	// 3 fixed rules (admin api, admin status, healthz) + 2 receivers
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
	mustHavePath(100, "/api/v1/*")
	mustHavePath(110, "/api/v1/status*")
	mustHavePath(120, "/healthz")
	mustHavePath(120, "/readyz")
	mustHavePath(130, "/hooks/webhook")
	mustHavePath(140, "/transport/http/receivers/events/messages")
}

func TestALBAttachment_Single_BothTGsTargetSingleService(t *testing.T) {
	defer jsii.Close()
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, baseYAML))
	single := newSingle(t, stack, vpc, src)
	gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single: single, Listener: listener, Vpc: vpc, BridgeConfig: src,
	})
	tpl := assertions.Template_FromStack(stack, nil)

	// Single facade ⇒ exactly 1 ECS service. Both TGs reference it
	// via the service's LoadBalancers[].TargetGroupArn — so the
	// service resource must have 2 LoadBalancers entries (one per
	// TG), confirming both attached.
	svcs := tpl.FindResources(jsii.String("AWS::ECS::Service"), nil)
	if len(*svcs) != 1 {
		t.Fatalf("ECS::Service count = %d, want 1", len(*svcs))
	}
	for _, raw := range *svcs {
		props := (*raw)["Properties"].(map[string]any)
		lbs, _ := props["LoadBalancers"].([]any)
		if len(lbs) != 2 {
			t.Fatalf("Single service LoadBalancers count = %d, want 2 (one per TG)", len(lbs))
		}
	}
}

func TestALBAttachment_Cluster_TGsTargetCorrectServices(t *testing.T) {
	defer jsii.Close()
	_, stack, vpc, listener := newApp(t)
	src := source.NewAsset(writeYAML(t, baseYAML))
	cluster := newCluster(t, stack, vpc, src)
	gobridgealbattachment.NewGoBridgeALBAttachment(stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Cluster: cluster, Listener: listener, Vpc: vpc, BridgeConfig: src,
	})
	tpl := assertions.Template_FromStack(stack, nil)

	svcs := tpl.FindResources(jsii.String("AWS::ECS::Service"), nil)
	if len(*svcs) != 2 {
		t.Fatalf("ECS::Service count = %d, want 2", len(*svcs))
	}
	for _, raw := range *svcs {
		props := (*raw)["Properties"].(map[string]any)
		lbs, _ := props["LoadBalancers"].([]any)
		if len(lbs) != 1 {
			t.Fatalf("each cluster service should attach to exactly 1 TG, got %d", len(lbs))
		}
	}
	tpl.ResourceCountIs(jsii.String("AWS::ElasticLoadBalancingV2::TargetGroup"), jsii.Number(2))
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
		if props["HealthCheckPath"] != "/healthz" {
			t.Fatalf("HealthCheckPath = %v, want /healthz", props["HealthCheckPath"])
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
