//go:build !race

package gobridgebase_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/jsii-runtime-go"

	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/gobridgebase"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/imgsource"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

const sampleYAML = `
bridge:
  id: test-bridge
`

func writeTempYAML(t *testing.T, body string) string {
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

func newScope(t *testing.T) (awscdk.Stack, awsec2.IVpc, *cdkconstructs.GoBridgeEfsConfig) {
	t.Helper()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("TestStack"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	efs := cdkconstructs.NewGoBridgeEfsConfig(stack, jsii.String("Efs"),
		&cdkconstructs.GoBridgeEfsConfigProps{Vpc: vpc})
	return stack, vpc, efs
}

func newBuilt(t *testing.T, mode gobridgebase.Mode, yaml string) (awscdk.Stack, *gobridgebase.Built) {
	t.Helper()
	stack, vpc, efs := newScope(t)
	src := source.NewAsset(writeTempYAML(t, yaml))
	b := gobridgebase.New(stack, jsii.String("Bridge"), &gobridgebase.Props{
		Mode:      mode,
		Vpc:       vpc,
		EfsConfig: efs,
		Image:     imgsource.NewRegistry("gobridge@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Bootstrap: bootstrap(),
		Source:    src,
	})
	return stack, b
}
