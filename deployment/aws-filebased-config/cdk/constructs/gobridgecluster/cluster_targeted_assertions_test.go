//go:build !race

package gobridgecluster_test

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
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgecluster"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

const t20ClusterYAML = `
bridge:
  id: test-bridge
`

func t20ClusterWriteYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return p
}

func t20ClusterBootstrap() infra.BootstrapConfig {
	return infra.BootstrapConfig{
		BridgeID:         "bridge-1",
		ConfigFilePath:   "/var/lib/gobridge/bridge.yaml",
		AdminAPIKeyParam: "/test/admin",
	}
}

func t20ClusterNew(t *testing.T) (awscdk.Stack, *gobridgecluster.GoBridgeCluster) {
	t.Helper()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("S"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	src := source.NewAsset(t20ClusterWriteYAML(t, t20ClusterYAML))
	g := gobridgecluster.NewGoBridgeCluster(stack, jsii.String("Bridge"), &gobridgecluster.ClusterProps{
		Vpc:          vpc,
		Image:        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap:    t20ClusterBootstrap(),
		BridgeConfig: src,
	})
	return stack, g
}

// Test_T20_Cluster_ResourceCounts: 2 ECS Services, 2 TaskDefinitions, and a
// log group per container per service (4 total: control main+seeder, worker
// main+seeder).
func Test_T20_Cluster_ResourceCounts(t *testing.T) {
	stack, _ := t20ClusterNew(t)
	tpl := assertions.Template_FromStack(stack, nil)

	tpl.ResourceCountIs(jsii.String("AWS::ECS::Service"), jsii.Number(2))
	tpl.ResourceCountIs(jsii.String("AWS::ECS::TaskDefinition"), jsii.Number(2))
	tpl.ResourceCountIs(jsii.String("AWS::Logs::LogGroup"), jsii.Number(4))

	groups := tpl.FindResources(jsii.String("AWS::Logs::LogGroup"), nil)
	var controlNames, workerNames []string
	for _, raw := range *groups {
		props := (*raw)["Properties"].(map[string]any)
		name, _ := props["LogGroupName"].(string)
		switch {
		case strings.Contains(name, "/Control"):
			controlNames = append(controlNames, name)
		case strings.Contains(name, "/Worker"):
			workerNames = append(workerNames, name)
		}
	}
	if len(controlNames) != 2 || len(workerNames) != 2 {
		t.Fatalf("expected 2 LogGroups per service, got control=%v worker=%v", controlNames, workerNames)
	}
}

// Test_T20_Cluster_Mounts_PerTaskDef walks each TaskDefinition independently
// and asserts the gobridge main container's MountPoint ReadOnly flag —
// control TD must be RW (false), worker TD must be RO (true). The existing
// suite asserts "one of each" via a sawRW/sawRO loop; this test pins each
// task def by its logical-id substring so a future regression that swaps
// the mount flags fails loudly.
func Test_T20_Cluster_Mounts_PerTaskDef(t *testing.T) {
	stack, _ := t20ClusterNew(t)
	tpl := assertions.Template_FromStack(stack, nil)
	tds := tpl.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)

	want := map[string]bool{"Control": false, "Worker": true}
	checked := map[string]bool{}
	for id, raw := range *tds {
		var role string
		switch {
		case strings.Contains(id, "Control"):
			role = "Control"
		case strings.Contains(id, "Worker"):
			role = "Worker"
		default:
			t.Fatalf("unknown TaskDefinition logical id %q", id)
		}
		props := (*raw)["Properties"].(map[string]any)
		cds := props["ContainerDefinitions"].([]any)
		for _, cd := range cds {
			m := cd.(map[string]any)
			if m["Name"] != "gobridge" {
				continue
			}
			mps := m["MountPoints"].([]any)
			ro, _ := mps[0].(map[string]any)["ReadOnly"].(bool)
			if ro != want[role] {
				t.Fatalf("%s TaskDef ReadOnly = %v, want %v", role, ro, want[role])
			}
			checked[role] = true
		}
	}
	if !checked["Control"] || !checked["Worker"] {
		t.Fatalf("did not assert both Control and Worker, checked=%v", checked)
	}
}

// Test_T20_Cluster_WorkerRole_NoClientWrite: the worker task role's IAM
// policy must contain ClientMount but NEVER ClientWrite. Iterate all
// AWS::IAM::Policy resources whose Roles ref points at a "*Worker*" role
// and assert the action set.
func Test_T20_Cluster_WorkerRole_NoClientWrite(t *testing.T) {
	stack, _ := t20ClusterNew(t)
	tpl := assertions.Template_FromStack(stack, nil)
	policies := tpl.FindResources(jsii.String("AWS::IAM::Policy"), nil)

	type bucket struct{ mount, write bool }
	per := map[string]*bucket{} // role hint -> bucket
	for _, raw := range *policies {
		props := (*raw)["Properties"].(map[string]any)
		roles, _ := props["Roles"].([]any)
		var roleHint string
		for _, r := range roles {
			if rm, ok := r.(map[string]any); ok {
				if ref, ok := rm["Ref"].(string); ok {
					switch {
					case strings.Contains(ref, "Worker"):
						roleHint = "Worker"
					case strings.Contains(ref, "Control"):
						roleHint = "Control"
					}
				}
			}
		}
		if roleHint == "" {
			continue
		}
		if per[roleHint] == nil {
			per[roleHint] = &bucket{}
		}
		doc, _ := props["PolicyDocument"].(map[string]any)
		stmts, _ := doc["Statement"].([]any)
		for _, s := range stmts {
			for _, a := range t20ClusterNormalizeActions(s.(map[string]any)["Action"]) {
				if a == "elasticfilesystem:ClientMount" {
					per[roleHint].mount = true
				}
				if a == "elasticfilesystem:ClientWrite" {
					per[roleHint].write = true
				}
			}
		}
	}
	if per["Worker"] == nil {
		t.Fatalf("did not find any IAM::Policy attached to a Worker role")
	}
	if !per["Worker"].mount {
		t.Fatalf("worker role missing elasticfilesystem:ClientMount")
	}
	if per["Worker"].write {
		t.Fatalf("worker role MUST NOT have elasticfilesystem:ClientWrite")
	}
	if per["Control"] != nil {
		if !per["Control"].mount || !per["Control"].write {
			t.Fatalf("control role expected ClientMount+ClientWrite, got %+v", per["Control"])
		}
	}
}

// Test_T20_Cluster_AdminPort_OnBothTaskDefs: every TaskDefinition exposes the
// admin port (8080/tcp) on the gobridge main container — required for both
// control AND worker because the cluster wiring health-checks both via ALB.
func Test_T20_Cluster_AdminPort_OnBothTaskDefs(t *testing.T) {
	stack, _ := t20ClusterNew(t)
	tpl := assertions.Template_FromStack(stack, nil)
	tds := tpl.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
	if len(*tds) != 2 {
		t.Fatalf("want 2 TaskDefinitions, got %d", len(*tds))
	}
	for id, raw := range *tds {
		props := (*raw)["Properties"].(map[string]any)
		cds := props["ContainerDefinitions"].([]any)
		var sawAdmin bool
		for _, cd := range cds {
			m := cd.(map[string]any)
			if m["Name"] != "gobridge" {
				continue
			}
			pms, _ := m["PortMappings"].([]any)
			for _, pm := range pms {
				pmm := pm.(map[string]any)
				port, _ := pmm["ContainerPort"].(float64)
				proto, _ := pmm["Protocol"].(string)
				if port == 8080 && proto == "tcp" {
					sawAdmin = true
				}
			}
		}
		if !sawAdmin {
			t.Fatalf("TaskDefinition %s missing admin 8080/tcp PortMapping", id)
		}
	}
}

func t20ClusterNormalizeActions(v any) []string {
	switch tv := v.(type) {
	case string:
		return []string{tv}
	case []any:
		out := make([]string, 0, len(tv))
		for _, e := range tv {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// resourceTags flattens a synthesized resource's Properties.Tags
// ([]{Key,Value}) into a key→value map.
func resourceTags(props map[string]any) map[string]string {
	out := map[string]string{}
	raw, _ := props["Tags"].([]any)
	for _, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		k, _ := m["Key"].(string)
		v, _ := m["Value"].(string)
		if k != "" {
			out[k] = v
		}
	}
	return out
}

// Test_T20_Cluster_AdvertisesNonHA is the c15-cluster-notha honesty
// guard. GoBridgeCluster is a filesystem-replicated SCALE-OUT topology,
// not coordinated-failover HA (it forces topology=filesystem_replicated,
// under which the runtime rejects shared_outbox / route.session, and
// there is no 30-60s failover path). The construct MUST advertise that
// non-HA nature in its SYNTHESIZED output — not just in doc comments —
// so an operator inspecting the deployed stack sees it:
//
//   - every ECS Service carries gobridge:topology and gobridge:ha tags
//     whose values say "scale-out" and "not coordinated failover";
//   - a synth-time info annotation spells out that this is not HA and
//     has no 30-60s failover path.
//
// Mutation: delete the Tags_Of(...).Add + Annotations_Of(...).AddInfo
// honesty block in NewGoBridgeCluster → the tags vanish and the info
// annotation disappears, and this test FAILs.
func Test_T20_Cluster_AdvertisesNonHA(t *testing.T) {
	stack, _ := t20ClusterNew(t)
	tpl := assertions.Template_FromStack(stack, nil)

	// (1) Both ECS services must carry the non-HA / scale-out tags.
	svcs := tpl.FindResources(jsii.String("AWS::ECS::Service"), nil)
	if svcs == nil || len(*svcs) != 2 {
		t.Fatalf("want 2 ECS Services, got %v", svcs)
	}
	for id, raw := range *svcs {
		tags := resourceTags((*raw)["Properties"].(map[string]any))
		if got := tags["gobridge:topology"]; got != "filesystem-replicated-scale-out" {
			t.Fatalf("service %s: gobridge:topology tag = %q, want %q (scale-out, not HA)",
				id, got, "filesystem-replicated-scale-out")
		}
		if got := tags["gobridge:ha"]; got != "none-scale-out-not-coordinated-failover" {
			t.Fatalf("service %s: gobridge:ha tag = %q, want %q (advertises NOT coordinated-failover HA)",
				id, got, "none-scale-out-not-coordinated-failover")
		}
	}

	// (2) A synth-time info annotation must spell out the non-HA nature.
	ann := assertions.Annotations_FromStack(stack)
	infos := ann.FindInfo(jsii.String("*"), assertions.Match_AnyValue())
	if infos == nil {
		t.Fatalf("no info annotations emitted; expected a non-HA advisory")
	}
	var found bool
	for _, m := range *infos {
		if m == nil || m.Entry == nil { //nolint:staticcheck // CDK assertions still surface SynthesisMessage; no Go replacement on Find* yet.
			continue
		}
		msg := fmt.Sprintf("%v", m.Entry.Data) //nolint:staticcheck // see above
		if strings.Contains(msg, "NOT coordinated-failover HA") &&
			strings.Contains(msg, "SCALE-OUT") &&
			strings.Contains(msg, "no 30-60s") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no info annotation communicating the non-HA (scale-out, no 30-60s failover) nature was found")
	}
}
