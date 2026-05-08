//go:build !race

package singleton_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/jsii-runtime-go"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgecluster"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgesingle"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/singleton"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

const sampleYAML = `
bridge:
  id: test-bridge
`

func writeYAML(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(p, []byte(sampleYAML), 0o600); err != nil {
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

func newSingle(t *testing.T, scope awscdk.Stack, id string) {
	t.Helper()
	vpc := awsec2.NewVpc(scope, jsii.String("Vpc-"+id), nil)
	gobridgesingle.NewGoBridgeSingle(scope, jsii.String(id), &gobridgesingle.SingleProps{
		Vpc:          vpc,
		Image:        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap:    bootstrap(),
		BridgeConfig: source.NewAsset(writeYAML(t)),
	})
}

func newCluster(t *testing.T, scope awscdk.Stack, id string) {
	t.Helper()
	vpc := awsec2.NewVpc(scope, jsii.String("Vpc-"+id), nil)
	gobridgecluster.NewGoBridgeCluster(scope, jsii.String(id), &gobridgecluster.ClusterProps{
		Vpc:          vpc,
		Image:        awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap:    bootstrap(),
		BridgeConfig: source.NewAsset(writeYAML(t)),
	})
}

func resetCleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(singleton.ResetForTest)
}

func TestSingleton_OneSingle_NoPanic(t *testing.T) {
	defer jsii.Close()
	resetCleanup(t)
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("S"), nil)
	newSingle(t, stack, "Bridge")
}

func TestSingleton_OneCluster_NoPanic(t *testing.T) {
	defer jsii.Close()
	resetCleanup(t)
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("S"), nil)
	newCluster(t, stack, "Bridge")
}

func TestSingleton_Zero_NoPanic(t *testing.T) {
	defer jsii.Close()
	resetCleanup(t)
	app := awscdk.NewApp(nil)
	awscdk.NewStack(app, jsii.String("S"), nil)
}

func TestSingleton_SingleAndCluster_SameStack_Panics(t *testing.T) {
	defer jsii.Close()
	resetCleanup(t)
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("S"), nil)
	newSingle(t, stack, "BridgeA")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on second facade in same Stack")
		}
		msg := fmt.Sprintf("%v", r)
		const wantPrefix = "Only one GoBridgeSingle or GoBridgeCluster instance is supported per stack/account; found 2."
		if !strings.HasPrefix(msg, wantPrefix) {
			t.Fatalf("panic prefix mismatch:\n  got: %q\n want: %q", msg, wantPrefix)
		}
		if !strings.Contains(msg, "BridgeA") {
			t.Fatalf("panic message must list BridgeA path; got: %s", msg)
		}
		if !strings.Contains(msg, "BridgeB") {
			t.Fatalf("panic message must list BridgeB path; got: %s", msg)
		}
		if !strings.Contains(msg, "single") || !strings.Contains(msg, "cluster") {
			t.Fatalf("panic message must list both kinds; got: %s", msg)
		}
		if !strings.Contains(msg, "Fix: remove the extra instance(s)") {
			t.Fatalf("panic message must include Fix line; got: %s", msg)
		}
	}()
	newCluster(t, stack, "BridgeB")
}

func TestSingleton_TwoSingle_SameStack_Panics(t *testing.T) {
	defer jsii.Close()
	resetCleanup(t)
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("S"), nil)
	newSingle(t, stack, "BridgeA")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on second GoBridgeSingle in same Stack")
		}
		msg := fmt.Sprintf("%v", r)
		const wantPrefix = "Only one GoBridgeSingle or GoBridgeCluster instance is supported per stack/account; found 2."
		if !strings.HasPrefix(msg, wantPrefix) {
			t.Fatalf("panic prefix mismatch:\n  got: %q\n want: %q", msg, wantPrefix)
		}
		if strings.Count(msg, "single") < 2 {
			t.Fatalf("panic message must list two single kinds; got: %s", msg)
		}
	}()
	newSingle(t, stack, "BridgeB")
}

func TestSingleton_DifferentStacks_NoPanic(t *testing.T) {
	defer jsii.Close()
	resetCleanup(t)
	app := awscdk.NewApp(nil)
	a := awscdk.NewStack(app, jsii.String("StackA"), nil)
	b := awscdk.NewStack(app, jsii.String("StackB"), nil)
	newSingle(t, a, "Bridge")
	newCluster(t, b, "Bridge")
}

func TestSingleton_ResetForTest_ClearsState(t *testing.T) {
	defer jsii.Close()
	resetCleanup(t)
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("S"), nil)
	newSingle(t, stack, "BridgeA")

	// Without ResetForTest a second registration in the same Stack
	// would push count to 2 and panic. We clear first to prove the
	// helper actually wipes state.
	singleton.ResetForTest()

	app2 := awscdk.NewApp(nil)
	stack2 := awscdk.NewStack(app2, jsii.String("S"), nil)
	newSingle(t, stack2, "BridgeA")
}
