//go:build !race

package gobridgealarms_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awscloudwatch"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	elbv2 "github.com/aws/aws-cdk-go/awscdk/v2/awselasticloadbalancingv2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssns"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealarms"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgealbattachment"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgecluster"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

const sampleYAML = `
bridge:
  id: test-bridge
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

type harness struct {
	app      awscdk.App
	stack    awscdk.Stack
	vpc      awsec2.IVpc
	listener elbv2.IApplicationListener
	topic    awssns.ITopic
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("S"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	alb := elbv2.NewApplicationLoadBalancer(stack, jsii.String("ALB"), &elbv2.ApplicationLoadBalancerProps{
		Vpc:            vpc,
		InternetFacing: jsii.Bool(true),
	})
	listener := alb.AddListener(jsii.String("L"), &elbv2.BaseApplicationListenerProps{
		Port:          jsii.Number(80),
		Protocol:      elbv2.ApplicationProtocol_HTTP,
		Open:          jsii.Bool(false),
		DefaultAction: elbv2.ListenerAction_FixedResponse(jsii.Number(404), nil),
	})
	topic := awssns.NewTopic(stack, jsii.String("Topic"), nil)
	return &harness{app: app, stack: stack, vpc: vpc, listener: listener, topic: topic}
}

func (h *harness) newSingle(t *testing.T) *gobridgesingle.GoBridgeSingle {
	t.Helper()
	src := source.NewAsset(writeYAML(t, sampleYAML))
	return gobridgesingle.NewGoBridgeSingle(h.stack, jsii.String("Single"), &gobridgesingle.SingleProps{
		Vpc:          h.vpc,
		Image:        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap:    bootstrap(),
		BridgeConfig: src,
	})
}

func (h *harness) newCluster(t *testing.T) *gobridgecluster.GoBridgeCluster {
	t.Helper()
	src := source.NewAsset(writeYAML(t, sampleYAML))
	return gobridgecluster.NewGoBridgeCluster(h.stack, jsii.String("Cluster"), &gobridgecluster.ClusterProps{
		Vpc:          h.vpc,
		Image:        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap:    bootstrap(),
		BridgeConfig: src,
	})
}

func (h *harness) newAttachment(t *testing.T, c *gobridgecluster.GoBridgeCluster) *gobridgealbattachment.GoBridgeALBAttachment {
	t.Helper()
	src := source.NewAsset(writeYAML(t, sampleYAML))
	return gobridgealbattachment.NewGoBridgeALBAttachment(h.stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Cluster:      c,
		Listener:     h.listener,
		Vpc:          h.vpc,
		BridgeConfig: src,
	})
}

func (h *harness) newSingleAttachment(t *testing.T, s *gobridgesingle.GoBridgeSingle) *gobridgealbattachment.GoBridgeALBAttachment {
	t.Helper()
	src := source.NewAsset(writeYAML(t, sampleYAML))
	return gobridgealbattachment.NewGoBridgeALBAttachment(h.stack, jsii.String("Att"), &gobridgealbattachment.AttachmentProps{
		Single:       s,
		Listener:     h.listener,
		Vpc:          h.vpc,
		BridgeConfig: src,
	})
}

func alarmCount(t *testing.T, stack awscdk.Stack) int {
	t.Helper()
	tpl := assertions.Template_FromStack(stack, nil)
	r := tpl.FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil)
	if r == nil {
		return 0
	}
	return len(*r)
}

func findAlarms(t *testing.T, stack awscdk.Stack) map[string]map[string]any {
	t.Helper()
	tpl := assertions.Template_FromStack(stack, nil)
	r := tpl.FindResources(jsii.String("AWS::CloudWatch::Alarm"), nil)
	out := map[string]map[string]any{}
	if r == nil {
		return out
	}
	for k, raw := range *r {
		out[k] = (*raw)["Properties"].(map[string]any)
	}
	return out
}

func TestAlarms_Cluster_WithAttachment_Emits7(t *testing.T) {
	defer jsii.Close()
	h := newHarness(t)
	c := h.newCluster(t)
	att := h.newAttachment(t, c)
	g := gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("BridgeAlarms"), &gobridgealarms.AlarmsProps{
		Cluster:    c,
		Efs:        c.EfsConfig(),
		Attachment: att,
		AlarmTopic: h.topic,
	})
	if got := alarmCount(t, h.stack); got != 7 {
		t.Fatalf("alarm count = %d, want 7", got)
	}
	for name, a := range map[string]awscloudwatch.IAlarm{
		"control": g.ControlAbsenceAlarm(),
		"worker":  g.WorkerDegradedAlarm(),
		"efs":     g.EfsIOAlarm(),
		"unhCtl":  g.AlbUnhealthyControlAlarm(),
		"unhWrk":  g.AlbUnhealthyWorkerAlarm(),
		"5xxCtl":  g.Alb5xxControlAlarm(),
		"5xxWrk":  g.Alb5xxWorkerAlarm(),
	} {
		if a == nil {
			t.Fatalf("alarm %q is nil", name)
		}
	}
}

func TestAlarms_Single_WithAttachment_Emits6(t *testing.T) {
	defer jsii.Close()
	h := newHarness(t)
	s := h.newSingle(t)
	att := h.newSingleAttachment(t, s)
	g := gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("BridgeAlarms"), &gobridgealarms.AlarmsProps{
		Single:     s,
		Efs:        s.EfsConfig(),
		Attachment: att,
		AlarmTopic: h.topic,
	})
	if got := alarmCount(t, h.stack); got != 6 {
		t.Fatalf("alarm count = %d, want 6", got)
	}
	if g.WorkerDegradedAlarm() != nil {
		t.Fatal("worker degraded alarm must be nil for Single")
	}
	for name, a := range map[string]awscloudwatch.IAlarm{
		"control": g.ControlAbsenceAlarm(),
		"efs":     g.EfsIOAlarm(),
		"unhCtl":  g.AlbUnhealthyControlAlarm(),
		"unhWrk":  g.AlbUnhealthyWorkerAlarm(),
		"5xxCtl":  g.Alb5xxControlAlarm(),
		"5xxWrk":  g.Alb5xxWorkerAlarm(),
	} {
		if a == nil {
			t.Fatalf("alarm %q is nil", name)
		}
	}
}

func TestAlarms_Single_NoAttachment_Emits2(t *testing.T) {
	defer jsii.Close()
	h := newHarness(t)
	s := h.newSingle(t)
	g := gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("BridgeAlarms"), &gobridgealarms.AlarmsProps{
		Single:     s,
		Efs:        s.EfsConfig(),
		AlarmTopic: h.topic,
	})
	if got := alarmCount(t, h.stack); got != 2 {
		t.Fatalf("alarm count = %d, want 2", got)
	}
	if g.ControlAbsenceAlarm() == nil {
		t.Fatal("control absence alarm should be set")
	}
	if g.WorkerDegradedAlarm() != nil {
		t.Fatal("worker degraded alarm must be nil for Single")
	}
	if g.EfsIOAlarm() == nil {
		t.Fatal("efs io alarm should be set")
	}
	if g.AlbUnhealthyControlAlarm() != nil || g.Alb5xxControlAlarm() != nil {
		t.Fatal("ALB alarms must be nil without Attachment")
	}
}

func TestAlarms_Cluster_NoAttachment_Emits3(t *testing.T) {
	defer jsii.Close()
	h := newHarness(t)
	c := h.newCluster(t)
	gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("BridgeAlarms"), &gobridgealarms.AlarmsProps{
		Cluster:    c,
		Efs:        c.EfsConfig(),
		AlarmTopic: h.topic,
	})
	if got := alarmCount(t, h.stack); got != 3 {
		t.Fatalf("alarm count = %d, want 3", got)
	}
}

func TestAlarms_ThresholdOverrides(t *testing.T) {
	defer jsii.Close()
	h := newHarness(t)
	c := h.newCluster(t)
	att := h.newAttachment(t, c)
	gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("BridgeAlarms"), &gobridgealarms.AlarmsProps{
		Cluster:                    c,
		Efs:                        c.EfsConfig(),
		Attachment:                 att,
		AlarmTopic:                 h.topic,
		EfsPercentIOLimitThreshold: jsii.Number(75),
		Alb5xxThreshold:            jsii.Number(42),
	})
	alarms := findAlarms(t, h.stack)

	wantThresholds := map[string]float64{
		"PercentIOLimit":            75,
		"HTTPCode_Target_5XX_Count": 42,
	}
	matched := map[string]int{}
	for _, props := range alarms {
		metricName, _ := props["MetricName"].(string)
		th, _ := props["Threshold"].(float64)
		if want, ok := wantThresholds[metricName]; ok {
			if th != want {
				t.Fatalf("metric %s threshold = %v want %v", metricName, th, want)
			}
			matched[metricName]++
		}
	}
	if matched["PercentIOLimit"] != 1 {
		t.Fatalf("PercentIOLimit alarm not found")
	}
	if matched["HTTPCode_Target_5XX_Count"] != 2 {
		t.Fatalf("5xx alarms found = %d want 2", matched["HTTPCode_Target_5XX_Count"])
	}
}

func TestAlarms_DisableEachAlarm(t *testing.T) {
	defer jsii.Close()
	cases := []struct {
		name      string
		mut       func(*gobridgealarms.AlarmsProps)
		wantCount int
		wantNil   func(g *gobridgealarms.GoBridgeAlarms) (string, bool)
	}{
		{
			name:      "control",
			mut:       func(p *gobridgealarms.AlarmsProps) { p.DisableControlAbsence = true },
			wantCount: 6,
			wantNil: func(g *gobridgealarms.GoBridgeAlarms) (string, bool) {
				return "control", g.ControlAbsenceAlarm() == nil
			},
		},
		{
			name:      "worker",
			mut:       func(p *gobridgealarms.AlarmsProps) { p.DisableWorkerDegraded = true },
			wantCount: 6,
			wantNil:   func(g *gobridgealarms.GoBridgeAlarms) (string, bool) { return "worker", g.WorkerDegradedAlarm() == nil },
		},
		{
			name:      "efs",
			mut:       func(p *gobridgealarms.AlarmsProps) { p.DisableEfsIO = true },
			wantCount: 6,
			wantNil:   func(g *gobridgealarms.GoBridgeAlarms) (string, bool) { return "efs", g.EfsIOAlarm() == nil },
		},
		{
			name:      "unhealthy",
			mut:       func(p *gobridgealarms.AlarmsProps) { p.DisableAlbUnhealthy = true },
			wantCount: 5,
			wantNil: func(g *gobridgealarms.GoBridgeAlarms) (string, bool) {
				return "unhealthy", g.AlbUnhealthyControlAlarm() == nil && g.AlbUnhealthyWorkerAlarm() == nil
			},
		},
		{
			name:      "5xx",
			mut:       func(p *gobridgealarms.AlarmsProps) { p.DisableAlb5xx = true },
			wantCount: 5,
			wantNil: func(g *gobridgealarms.GoBridgeAlarms) (string, bool) {
				return "5xx", g.Alb5xxControlAlarm() == nil && g.Alb5xxWorkerAlarm() == nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			c := h.newCluster(t)
			att := h.newAttachment(t, c)
			props := &gobridgealarms.AlarmsProps{
				Cluster:    c,
				Efs:        c.EfsConfig(),
				Attachment: att,
				AlarmTopic: h.topic,
			}
			tc.mut(props)
			g := gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("BridgeAlarms"), props)
			if got := alarmCount(t, h.stack); got != tc.wantCount {
				t.Fatalf("alarm count = %d, want %d", got, tc.wantCount)
			}
			if name, ok := tc.wantNil(g); !ok {
				t.Fatalf("expected %s alarm(s) to be nil", name)
			}
		})
	}
}

func TestAlarms_Validation_PanicMessages(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(h *harness) *gobridgealarms.AlarmsProps
		expect string
	}{
		{
			name: "neither",
			setup: func(h *harness) *gobridgealarms.AlarmsProps {
				s := h.newSingle(t)
				return &gobridgealarms.AlarmsProps{Efs: s.EfsConfig(), AlarmTopic: h.topic}
			},
			expect: "GoBridgeAlarms requires exactly one of Single or Cluster (found 0). Pass the facade you instantiated.",
		},
		{
			name: "both",
			setup: func(h *harness) *gobridgealarms.AlarmsProps {
				s := h.newSingle(t)
				// Cluster needs its own stack — singleton enforcement
				// forbids both facades in the same stack.
				stack2 := awscdk.NewStack(h.app, jsii.String("S2"), nil)
				vpc2 := awsec2.NewVpc(stack2, jsii.String("Vpc2"), nil)
				src := source.NewAsset(writeYAML(t, sampleYAML))
				c := gobridgecluster.NewGoBridgeCluster(stack2, jsii.String("Cluster"), &gobridgecluster.ClusterProps{
					Vpc:          vpc2,
					Image:        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
					Bootstrap:    bootstrap(),
					BridgeConfig: src,
				})
				return &gobridgealarms.AlarmsProps{Single: s, Cluster: c, Efs: c.EfsConfig(), AlarmTopic: h.topic}
			},
			expect: "GoBridgeAlarms requires exactly one of Single or Cluster (found 2). Pass the facade you instantiated.",
		},
		{
			name: "no efs",
			setup: func(h *harness) *gobridgealarms.AlarmsProps {
				s := h.newSingle(t)
				return &gobridgealarms.AlarmsProps{Single: s, AlarmTopic: h.topic}
			},
			expect: "GoBridgeAlarms.Efs is required. Pass <facade>.EfsConfig().",
		},
		{
			name: "no topic",
			setup: func(h *harness) *gobridgealarms.AlarmsProps {
				s := h.newSingle(t)
				return &gobridgealarms.AlarmsProps{Single: s, Efs: s.EfsConfig()}
			},
			expect: "GoBridgeAlarms.AlarmTopic is required.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer jsii.Close()
			h := newHarness(t)
			props := tc.setup(h)
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("expected panic %q", tc.expect)
				}
				msg, _ := r.(string)
				if msg == "" {
					if e, ok := r.(error); ok {
						msg = e.Error()
					}
				}
				if !strings.Contains(msg, tc.expect) {
					t.Fatalf("panic = %q, want substring %q", msg, tc.expect)
				}
			}()
			gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("BridgeAlarms"), props)
		})
	}
}

func TestAlarms_EfsAlarm_DefaultThresholdAndMetric(t *testing.T) {
	defer jsii.Close()
	h := newHarness(t)
	c := h.newCluster(t)
	gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("BridgeAlarms"), &gobridgealarms.AlarmsProps{
		Cluster: c, Efs: c.EfsConfig(), AlarmTopic: h.topic,
	})
	alarms := findAlarms(t, h.stack)
	found := false
	for _, p := range alarms {
		if mn, _ := p["MetricName"].(string); mn == "PercentIOLimit" {
			found = true
			ns, _ := p["Namespace"].(string)
			if ns != "AWS/EFS" {
				t.Fatalf("namespace = %s want AWS/EFS", ns)
			}
			if th, _ := p["Threshold"].(float64); th != 90 {
				t.Fatalf("default threshold = %v want 90", th)
			}
		}
	}
	if !found {
		t.Fatal("PercentIOLimit alarm not found")
	}
}

func TestAlarms_OkActionWired(t *testing.T) {
	defer jsii.Close()
	h := newHarness(t)
	s := h.newSingle(t)
	gobridgealarms.NewGoBridgeAlarms(h.stack, jsii.String("BridgeAlarms"), &gobridgealarms.AlarmsProps{
		Single: s, Efs: s.EfsConfig(), AlarmTopic: h.topic,
	})
	alarms := findAlarms(t, h.stack)
	if len(alarms) == 0 {
		t.Fatal("no alarms emitted")
	}
	for k, p := range alarms {
		ok, _ := p["OKActions"].([]any)
		if len(ok) != 1 {
			t.Fatalf("alarm %s OKActions count = %d want 1", k, len(ok))
		}
		al, _ := p["AlarmActions"].([]any)
		if len(al) != 1 {
			t.Fatalf("alarm %s AlarmActions count = %d want 1", k, len(al))
		}
	}
}
