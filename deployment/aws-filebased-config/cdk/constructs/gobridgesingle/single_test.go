//go:build !race

package gobridgesingle_test

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

	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

const singleSampleYAML = `
bridge:
  id: test-bridge
`

func writeSingleYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return p
}

func singleBootstrap() infra.BootstrapConfig {
	return infra.BootstrapConfig{
		BridgeID:         "bridge-1",
		ConfigFilePath:   "/var/lib/gobridge/bridge.yaml",
		AdminAPIKeyParam: "/test/admin",
	}
}

func newSingleStack(t *testing.T) (awscdk.Stack, *gobridgesingle.GoBridgeSingle) {
	t.Helper()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("SingleStack"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	src := source.NewAsset(writeSingleYAML(t, singleSampleYAML))
	g := gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Bridge"), &gobridgesingle.SingleProps{
		Vpc:          vpc,
		Image:        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap:    singleBootstrap(),
		BridgeConfig: src,
	})
	return stack, g
}

func TestGoBridgeSingle_Synth_NoPanic_ServiceCount_DesiredCount(t *testing.T) {
	stack, g := newSingleStack(t)
	if g == nil {
		t.Fatal("constructor returned nil")
	}
	tpl := assertions.Template_FromStack(stack, nil)

	// Exactly 1 ECS service, 1 task def.
	tpl.ResourceCountIs(jsii.String("AWS::ECS::Service"), jsii.Number(1))
	tpl.ResourceCountIs(jsii.String("AWS::ECS::TaskDefinition"), jsii.Number(1))

	svcs := tpl.FindResources(jsii.String("AWS::ECS::Service"), nil)
	if svcs == nil || len(*svcs) != 1 {
		t.Fatalf("expected 1 ECS::Service, got %v", svcs)
	}
	for _, raw := range *svcs {
		props := (*raw)["Properties"].(map[string]any)
		if dc, ok := props["DesiredCount"].(float64); !ok || dc != 1 {
			t.Fatalf("DesiredCount = %v, want 1", props["DesiredCount"])
		}
		dc := props["DeploymentConfiguration"].(map[string]any)
		if mh, _ := dc["MinimumHealthyPercent"].(float64); mh != 0 {
			t.Fatalf("MinimumHealthyPercent = %v, want 0", dc["MinimumHealthyPercent"])
		}
		if mx, _ := dc["MaximumPercent"].(float64); mx != 100 {
			t.Fatalf("MaximumPercent = %v, want 100", dc["MaximumPercent"])
		}
	}
}

func TestGoBridgeSingle_MainContainer_RWMount(t *testing.T) {
	stack, _ := newSingleStack(t)
	tpl := assertions.Template_FromStack(stack, nil)
	tds := tpl.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
	for _, raw := range *tds {
		props := (*raw)["Properties"].(map[string]any)
		cds := props["ContainerDefinitions"].([]any)
		var sawMain bool
		for _, cd := range cds {
			m := cd.(map[string]any)
			if m["Name"] != "gobridge" {
				continue
			}
			sawMain = true
			mps := m["MountPoints"].([]any)
			if len(mps) != 1 {
				t.Fatalf("want 1 mount point, got %d", len(mps))
			}
			ro, _ := mps[0].(map[string]any)["ReadOnly"].(bool)
			if ro {
				t.Fatalf("control main mount must be RW, got ReadOnly=true")
			}
		}
		if !sawMain {
			t.Fatal("did not find gobridge main container")
		}
	}
}

func TestGoBridgeSingle_Accessors(t *testing.T) {
	_, g := newSingleStack(t)
	if g.ControlService() == nil {
		t.Fatal("ControlService() nil")
	}
	if g.TaskDefinition() == nil {
		t.Fatal("TaskDefinition() nil")
	}
	if g.Cluster() == nil {
		t.Fatal("Cluster() nil")
	}
	if g.EfsConfig() == nil {
		t.Fatal("EfsConfig() nil")
	}
	if g.SecurityGroup() == nil {
		t.Fatal("SecurityGroup() nil")
	}
	if len(g.PortMappings()) == 0 {
		t.Fatal("PortMappings empty")
	}
}

func TestGoBridgeSingle_NilProps_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil props")
		}
	}()
	gobridgesingle.NewGoBridgeSingle(nil, jsii.String("X"), nil)
}

func TestGoBridgeSingle_MissingRequired_Panics(t *testing.T) {
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("S"), nil)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when Image missing")
		}
	}()
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Bridge"), &gobridgesingle.SingleProps{
		Vpc:          vpc,
		Bootstrap:    singleBootstrap(),
		BridgeConfig: source.NewAsset(writeSingleYAML(t, singleSampleYAML)),
	})
}

func TestGoBridgeSingle_Phase1Failure_Panics(t *testing.T) {
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("S"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	// Bridge ID with invalid char fails Phase 1 bridge-id regex.
	bad := `
bridge:
  id: "bad id with spaces!"
`
	src := source.NewAsset(writeSingleYAML(t, bad))
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
	gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Bridge"), &gobridgesingle.SingleProps{
		Vpc:          vpc,
		Image:        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap:    singleBootstrap(),
		BridgeConfig: src,
	})
}

// TestGoBridgeSingle_SuppliedEfsConfig_SubnetMismatchFailsSynth is the
// facade half of Validation Matrix row 14. The parity check itself is
// unit-tested in constructs/efs_config_validation_test.go; this proves
// GoBridgeSingle actually CALLS it on the supplied-EfsConfig path — the
// only path where the mismatch is possible, since an auto-created config
// receives props.VpcSubnets verbatim.
func TestGoBridgeSingle_SuppliedEfsConfig_SubnetMismatchFailsSynth(t *testing.T) {
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("SingleStack"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)

	azs := *vpc.AvailabilityZones()
	if len(azs) < 2 {
		t.Fatalf("fixture needs a multi-AZ VPC, got %d zone(s)", len(azs))
	}

	// Mount targets in the first AZ only; ECS placement left at the
	// default, which spans every private subnet — including the second AZ.
	efs := cdkconstructs.NewGoBridgeEfsConfig(stack, jsii.String("Efs"),
		&cdkconstructs.GoBridgeEfsConfigProps{
			Vpc: vpc,
			VpcSubnets: &awsec2.SubnetSelection{
				SubnetType:        awsec2.SubnetType_PRIVATE_WITH_EGRESS,
				AvailabilityZones: &[]*string{azs[0]},
			},
		})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("GoBridgeSingle must reject an EfsConfig that cannot serve its ECS placement")
		}
		if msg := fmt.Sprintf("%v", r); !strings.Contains(msg, "GoBridgeSingle") ||
			!strings.Contains(msg, *azs[1]) {
			t.Fatalf("panic must name the construct and the uncovered AZ %q, got: %s", *azs[1], msg)
		}
	}()

	gobridgesingle.NewGoBridgeSingle(stack, jsii.String("Bridge"), &gobridgesingle.SingleProps{
		Vpc:          vpc,
		EfsConfig:    efs,
		Image:        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap:    singleBootstrap(),
		BridgeConfig: source.NewAsset(writeSingleYAML(t, singleSampleYAML)),
	})
}
