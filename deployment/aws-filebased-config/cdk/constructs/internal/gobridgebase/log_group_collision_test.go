//go:build !race

package gobridgebase_test

import (
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/jsii-runtime-go"

	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/gobridgebase"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
)

// A log-group name is unique per account and region, and every facade builds
// its base under a fixed construct id. Two deployments of the same shape —
// a staging bridge beside a production one — therefore want the same name
// unless it is scoped to the stack that owns it, and the second stack fails
// at CREATE with an error naming the log group rather than the collision.

// logGroupNamesOfStack builds one base in a stack of the given name and
// returns the log-group names its template declares.
func logGroupNamesOfStack(t *testing.T, stackName string) []string {
	t.Helper()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String(stackName), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	efs := cdkconstructs.NewGoBridgeEfsConfig(stack, jsii.String("Efs"),
		&cdkconstructs.GoBridgeEfsConfigProps{Vpc: vpc})
	src := source.NewAsset(writeTempYAML(t, sampleYAML))
	gobridgebase.New(stack, jsii.String("Base"), &gobridgebase.Props{
		Mode:      gobridgebase.ModeControl,
		Vpc:       vpc,
		EfsConfig: efs,
		Image:     awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap: bootstrap(),
		Source:    src,
	})

	var names []string
	for _, raw := range *assertions.Template_FromStack(stack, nil).
		FindResources(jsii.String("AWS::Logs::LogGroup"), nil) {
		props := (*raw)["Properties"].(map[string]any)
		name, ok := props["LogGroupName"].(string)
		if !ok {
			t.Fatalf("log group in stack %s has no literal name: %v", stackName, props["LogGroupName"])
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		t.Fatalf("stack %s declares no log group", stackName)
	}
	return names
}

func TestNew_LogGroupNames_DoNotCollideAcrossDeploymentsOfTheSameShape(t *testing.T) {
	staging := logGroupNamesOfStack(t, "StagingBridge")
	production := logGroupNamesOfStack(t, "ProductionBridge")

	taken := map[string]bool{}
	for _, name := range staging {
		taken[name] = true
	}
	for _, name := range production {
		if taken[name] {
			t.Errorf("both deployments want the log group %s, so the second stack cannot create", name)
		}
	}

	// The name still says which deployment and which container it belongs to,
	// because that is what an operator reads it for.
	for _, name := range staging {
		if !strings.HasPrefix(name, "/gobridge/StagingBridge/Base/") {
			t.Errorf("log group %s does not name its stack and construct", name)
		}
	}
}
