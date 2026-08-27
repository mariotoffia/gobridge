//go:build !race

package gobridgecluster_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgecluster"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

const clusterSampleYAML = `
bridge:
  id: test-bridge
`

func writeClusterYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return p
}

func clusterBootstrap() infra.BootstrapConfig {
	return infra.BootstrapConfig{
		BridgeID:         "bridge-1",
		ConfigFilePath:   "/var/lib/gobridge/bridge.yaml",
		AdminAPIKeyParam: "/test/admin",
	}
}

func newClusterStack(t *testing.T, mut func(*gobridgecluster.ClusterProps)) (awscdk.Stack, *gobridgecluster.GoBridgeCluster) {
	t.Helper()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("ClusterStack"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	src := source.NewAsset(writeClusterYAML(t, clusterSampleYAML))
	props := &gobridgecluster.ClusterProps{
		Vpc:          vpc,
		Image:        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap:    clusterBootstrap(),
		BridgeConfig: src,
	}
	if mut != nil {
		mut(props)
	}
	g := gobridgecluster.NewGoBridgeCluster(stack, jsii.String("Bridge"), props)
	return stack, g
}

func envFor(envs []any, name string) string {
	for _, e := range envs {
		m := e.(map[string]any)
		if m["Name"] != name {
			continue
		}
		if v, ok := m["Value"].(string); ok {
			return v
		}
		return "<intrinsic>"
	}
	return ""
}

func mainContainer(t *testing.T, td map[string]any) map[string]any {
	t.Helper()
	props := td["Properties"].(map[string]any)
	cds := props["ContainerDefinitions"].([]any)
	for _, cd := range cds {
		m := cd.(map[string]any)
		if m["Name"] == "gobridge" {
			return m
		}
	}
	t.Fatalf("gobridge main container not found")
	return nil
}

func TestGoBridgeCluster_Synth_NoPanic_ServiceCount_TwoServices_TwoTaskDefs(t *testing.T) {
	stack, g := newClusterStack(t, nil)
	if g == nil {
		t.Fatal("constructor returned nil")
	}
	tpl := assertions.Template_FromStack(stack, nil)
	tpl.ResourceCountIs(jsii.String("AWS::ECS::Service"), jsii.Number(2))
	tpl.ResourceCountIs(jsii.String("AWS::ECS::TaskDefinition"), jsii.Number(2))
}

func TestGoBridgeCluster_Control_DesiredCount_And_DeploymentPolicy_0_100(t *testing.T) {
	stack, g := newClusterStack(t, nil)
	tpl := assertions.Template_FromStack(stack, nil)
	svcs := tpl.FindResources(jsii.String("AWS::ECS::Service"), nil)
	controlTDRef := *g.ControlTaskDefinition().TaskDefinitionArn()
	_ = controlTDRef
	var sawControl bool
	for _, raw := range *svcs {
		props := (*raw)["Properties"].(map[string]any)
		dc, _ := props["DesiredCount"].(float64)
		dep, _ := props["DeploymentConfiguration"].(map[string]any)
		mh, _ := dep["MinimumHealthyPercent"].(float64)
		mx, _ := dep["MaximumPercent"].(float64)
		if dc == 1 && mh == 0 && mx == 100 {
			sawControl = true
		}
	}
	if !sawControl {
		t.Fatal("expected exactly one service with DesiredCount=1 and 0/100 deployment policy")
	}
}

func TestGoBridgeCluster_Worker_DesiredCount_Default_2(t *testing.T) {
	stack, _ := newClusterStack(t, nil)
	tpl := assertions.Template_FromStack(stack, nil)
	svcs := tpl.FindResources(jsii.String("AWS::ECS::Service"), nil)
	var sawWorker bool
	for _, raw := range *svcs {
		props := (*raw)["Properties"].(map[string]any)
		if dc, _ := props["DesiredCount"].(float64); dc == 2 {
			sawWorker = true
		}
	}
	if !sawWorker {
		t.Fatal("expected a worker service with DesiredCount=2")
	}
}

func TestGoBridgeCluster_Worker_DesiredCount_Override(t *testing.T) {
	stack, _ := newClusterStack(t, func(p *gobridgecluster.ClusterProps) {
		v := 5.0
		p.WorkerDesiredCount = &v
	})
	tpl := assertions.Template_FromStack(stack, nil)
	svcs := tpl.FindResources(jsii.String("AWS::ECS::Service"), nil)
	var saw5 bool
	for _, raw := range *svcs {
		props := (*raw)["Properties"].(map[string]any)
		if dc, _ := props["DesiredCount"].(float64); dc == 5 {
			saw5 = true
		}
	}
	if !saw5 {
		t.Fatal("expected worker DesiredCount=5 after override")
	}
}

func TestGoBridgeCluster_Mounts_Control_RW_Worker_RO(t *testing.T) {
	stack, _ := newClusterStack(t, nil)
	tpl := assertions.Template_FromStack(stack, nil)
	tds := tpl.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
	var sawRW, sawRO bool
	for _, raw := range *tds {
		m := mainContainer(t, *raw)
		mps := m["MountPoints"].([]any)
		if len(mps) != 1 {
			t.Fatalf("want 1 mount point, got %d", len(mps))
		}
		ro, _ := mps[0].(map[string]any)["ReadOnly"].(bool)
		if ro {
			sawRO = true
		} else {
			sawRW = true
		}
	}
	if !sawRW || !sawRO {
		t.Fatalf("expected one RW and one RO main mount, sawRW=%v sawRO=%v", sawRW, sawRO)
	}
}

func TestGoBridgeCluster_NodeRole_Env_Wired(t *testing.T) {
	stack, _ := newClusterStack(t, nil)
	tpl := assertions.Template_FromStack(stack, nil)
	tds := tpl.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
	roles := map[string]bool{}
	for _, raw := range *tds {
		m := mainContainer(t, *raw)
		envs, _ := m["Environment"].([]any)
		role := envFor(envs, "GOBRIDGE_NODE_ROLE")
		if role == "" {
			t.Fatalf("GOBRIDGE_NODE_ROLE missing on main container")
		}
		roles[role] = true
	}
	if !roles["control"] || !roles["worker"] {
		t.Fatalf("expected both control and worker NodeRole envs, got %v", roles)
	}
}

func TestGoBridgeCluster_AutoScaling_Off_By_Default(t *testing.T) {
	stack, _ := newClusterStack(t, nil)
	tpl := assertions.Template_FromStack(stack, nil)
	tpl.ResourceCountIs(jsii.String("AWS::ApplicationAutoScaling::ScalableTarget"), jsii.Number(0))
	tpl.ResourceCountIs(jsii.String("AWS::ApplicationAutoScaling::ScalingPolicy"), jsii.Number(0))
}

// TestGoBridgeCluster_Topology_Forced_FilesystemReplicated asserts the cluster
// stamps topology=filesystem_replicated into the bootstrap JSON of BOTH task
// definitions even when the caller left the default. Without this the synth
// validator and runtime guard return early on "single" and shared_outbox /
// session leases on SQLite-over-EFS are silently permitted.
func TestGoBridgeCluster_Topology_Forced_FilesystemReplicated(t *testing.T) {
	// Caller intentionally leaves Topology unset (normalizes to "single").
	stack, _ := newClusterStack(t, nil)
	tpl := assertions.Template_FromStack(stack, nil)
	tds := tpl.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
	if len(*tds) != 2 {
		t.Fatalf("expected 2 task defs, got %d", len(*tds))
	}
	for _, raw := range *tds {
		m := mainContainer(t, *raw)
		envs, _ := m["Environment"].([]any)
		bj := envFor(envs, "GOBRIDGE_FILEBASED_BOOTSTRAP_JSON")
		if bj == "" || bj == "<intrinsic>" {
			t.Fatalf("bootstrap JSON env missing/intrinsic: %q", bj)
		}
		var cfg struct {
			Topology string `json:"topology"`
		}
		if err := json.Unmarshal([]byte(bj), &cfg); err != nil {
			t.Fatalf("unmarshal bootstrap JSON: %v", err)
		}
		if cfg.Topology != string(infra.TopologyFilesystemReplicated) {
			t.Fatalf("topology = %q, want %q", cfg.Topology, infra.TopologyFilesystemReplicated)
		}
	}
}

func TestGoBridgeCluster_AutoScaling_On_When_Set(t *testing.T) {
	stack, _ := newClusterStack(t, func(p *gobridgecluster.ClusterProps) {
		p.AutoScaling = &gobridgecluster.AutoScalingProps{Min: 2, Max: 8}
	})
	tpl := assertions.Template_FromStack(stack, nil)
	tpl.ResourceCountIs(jsii.String("AWS::ApplicationAutoScaling::ScalableTarget"), jsii.Number(1))
	tpl.ResourceCountIs(jsii.String("AWS::ApplicationAutoScaling::ScalingPolicy"), jsii.Number(1))

	pols := tpl.FindResources(jsii.String("AWS::ApplicationAutoScaling::ScalingPolicy"), nil)
	for _, raw := range *pols {
		props := (*raw)["Properties"].(map[string]any)
		conf := props["TargetTrackingScalingPolicyConfiguration"].(map[string]any)
		if tv, _ := conf["TargetValue"].(float64); tv != 70 {
			t.Fatalf("default TargetCPU should be 70, got %v", conf["TargetValue"])
		}
	}
}

func TestGoBridgeCluster_NilProps_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil props")
		}
	}()
	gobridgecluster.NewGoBridgeCluster(nil, jsii.String("X"), nil)
}

func TestGoBridgeCluster_MissingRequired_Panics(t *testing.T) {
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("S"), nil)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when Image missing")
		}
	}()
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	gobridgecluster.NewGoBridgeCluster(stack, jsii.String("Bridge"), &gobridgecluster.ClusterProps{
		Vpc:          vpc,
		Bootstrap:    clusterBootstrap(),
		BridgeConfig: source.NewAsset(writeClusterYAML(t, clusterSampleYAML)),
	})
}

func TestGoBridgeCluster_Phase1Failure_Panics(t *testing.T) {
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("S"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	bad := `
bridge:
  id: "bad id with spaces!"
`
	src := source.NewAsset(writeClusterYAML(t, bad))
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on Phase 1 validation failure")
		}
		msg := fmt.Sprintf("%v", r)
		if !strings.Contains(msg, "Phase 1") {
			t.Fatalf("expected panic message to contain %q, got: %s", "Phase 1", msg)
		}
	}()
	gobridgecluster.NewGoBridgeCluster(stack, jsii.String("Bridge"), &gobridgecluster.ClusterProps{
		Vpc:          vpc,
		Image:        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap:    clusterBootstrap(),
		BridgeConfig: src,
	})
}

func TestGoBridgeCluster_Accessors(t *testing.T) {
	_, g := newClusterStack(t, nil)
	if g.ControlService() == nil {
		t.Fatal("ControlService nil")
	}
	if g.WorkerService() == nil {
		t.Fatal("WorkerService nil")
	}
	if g.ControlTaskDefinition() == nil {
		t.Fatal("ControlTaskDefinition nil")
	}
	if g.WorkerTaskDefinition() == nil {
		t.Fatal("WorkerTaskDefinition nil")
	}
	if g.Cluster() == nil {
		t.Fatal("Cluster nil")
	}
	if g.EfsConfig() == nil {
		t.Fatal("EfsConfig nil")
	}
	if g.ControlSecurityGroup() == nil {
		t.Fatal("ControlSecurityGroup nil")
	}
	if g.WorkerSecurityGroup() == nil {
		t.Fatal("WorkerSecurityGroup nil")
	}
	if len(g.ControlPortMappings()) == 0 {
		t.Fatal("ControlPortMappings empty")
	}
	if len(g.WorkerPortMappings()) == 0 {
		t.Fatal("WorkerPortMappings empty")
	}
}
