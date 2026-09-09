//go:build !race

package gobridgealbattachment_test

import (
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
)

func TestALBAttachment_PortsPerConcern(t *testing.T) {
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

// TestALBAttachment_Ports_IgnoreHTTPOverride_MatchBootstrap guards port agreement
// at the synthesized-resource level: the bridge yaml sets an `http:` block
// (admin_addr :9090, monitor_addr :9091) that the file-based runtime IGNORES
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
