//go:build !race

package gobridgesingle_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

const t20SingleYAML = `
bridge:
  id: test-bridge
`

func t20SingleWriteYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return p
}

func t20SingleBootstrap() infra.BootstrapConfig {
	return infra.BootstrapConfig{
		BridgeID:         "bridge-1",
		ConfigFilePath:   "/var/lib/gobridge/bridge.yaml",
		AdminAPIKeyParam: "/test/admin",
	}
}

func t20SingleNew(t *testing.T) (awscdk.Stack, *gobridgesingle.GoBridgeSingle) {
	t.Helper()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("S"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	src := source.NewAsset(t20SingleWriteYAML(t, t20SingleYAML))
	g := gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Bridge"), &gobridgesingle.SingleProps{
		Vpc:          vpc,
		Image:        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap:    t20SingleBootstrap(),
		BridgeConfig: src,
	})
	return stack, g
}

// Test_T20_Single_ResourceCounts: 1 ECS Service, 1 TaskDefinition, no
// "Worker"-named services. Existing single_test asserts the service count
// but not the absence of any "Worker" logical id, which guards against a
// future regression where the cluster wiring leaks into the single facade.
func Test_T20_Single_ResourceCounts(t *testing.T) {
	defer jsii.Close()
	stack, _ := t20SingleNew(t)
	tpl := assertions.Template_FromStack(stack, nil)

	tpl.ResourceCountIs(jsii.String("AWS::ECS::Service"), jsii.Number(1))
	tpl.ResourceCountIs(jsii.String("AWS::ECS::TaskDefinition"), jsii.Number(1))

	svcs := tpl.FindResources(jsii.String("AWS::ECS::Service"), nil)
	for id := range *svcs {
		if containsCI(id, "Worker") {
			t.Fatalf("Single facade emitted a Worker-named ECS::Service %q", id)
		}
	}
}

// Test_T20_Single_MainContainer_RWMount: the single service main container's
// MountPoint MUST be RW (control-style mount).
func Test_T20_Single_MainContainer_RWMount(t *testing.T) {
	defer jsii.Close()
	stack, _ := t20SingleNew(t)
	tpl := assertions.Template_FromStack(stack, nil)
	tds := tpl.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
	if len(*tds) != 1 {
		t.Fatalf("want 1 TaskDefinition, got %d", len(*tds))
	}
	for _, raw := range *tds {
		props := (*raw)["Properties"].(map[string]any)
		cds := props["ContainerDefinitions"].([]any)
		for _, cd := range cds {
			m := cd.(map[string]any)
			if m["Name"] != "gobridge" {
				continue
			}
			mps := m["MountPoints"].([]any)
			if len(mps) != 1 {
				t.Fatalf("want 1 MountPoint, got %d", len(mps))
			}
			ro, _ := mps[0].(map[string]any)["ReadOnly"].(bool)
			if ro {
				t.Fatalf("Single main mount must be RW (ReadOnly=false), got true")
			}
		}
	}
}

// Test_T20_Single_AdminPort_OnTaskDef: the admin port (8080 by bootstrap
// default) must be present in the TaskDefinition's ContainerDefinitions
// PortMappings on tcp.
func Test_T20_Single_AdminPort_OnTaskDef(t *testing.T) {
	defer jsii.Close()
	stack, _ := t20SingleNew(t)
	tpl := assertions.Template_FromStack(stack, nil)
	tds := tpl.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
	for _, raw := range *tds {
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
			t.Fatalf("expected admin port 8080/tcp on main container PortMappings")
		}
	}
}

// Test_T20_Single_TaskRole_HasEFSClientWrite asserts the control-style task
// role policy emitted by the Single facade includes
// elasticfilesystem:ClientWrite (single = control).
func Test_T20_Single_TaskRole_HasEFSClientWrite(t *testing.T) {
	defer jsii.Close()
	stack, _ := t20SingleNew(t)
	tpl := assertions.Template_FromStack(stack, nil)

	tpl.HasResourceProperties(jsii.String("AWS::IAM::Policy"), map[string]any{
		"PolicyDocument": assertions.Match_ObjectLike(&map[string]any{
			"Statement": assertions.Match_ArrayWith(&[]any{
				assertions.Match_ObjectLike(&map[string]any{
					"Action": assertions.Match_ArrayWith(&[]any{
						"elasticfilesystem:ClientMount",
						"elasticfilesystem:ClientWrite",
					}),
				}),
			}),
		}),
	})
}

func containsCI(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(s) < len(sub) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			a, b := s[i+j], sub[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
