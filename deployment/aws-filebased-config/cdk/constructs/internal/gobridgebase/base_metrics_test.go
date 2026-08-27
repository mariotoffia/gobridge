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
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

// metricsBuild synthesizes a base construct with the given bootstrap so the
// metrics tests can toggle MetricsExporter independently of t20BaseBootstrap.
func metricsBuild(t *testing.T, boot infra.BootstrapConfig, memoryMiB ...float64) awscdk.Stack {
	t.Helper()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("S"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	efs := cdkconstructs.NewGoBridgeEfsConfig(stack, jsii.String("Efs"),
		&cdkconstructs.GoBridgeEfsConfigProps{Vpc: vpc})
	src := source.NewAsset(t20BaseWriteYAML(t, t20BaseSampleYAML))
	props := &gobridgebase.Props{
		Mode:      gobridgebase.ModeControl,
		Vpc:       vpc,
		EfsConfig: efs,
		Image:     awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap: boot,
		Source:    src,
	}
	if len(memoryMiB) > 0 {
		props.MemoryMiB = &memoryMiB[0]
	}
	gobridgebase.New(stack, jsii.String("Bridge"), props)
	return stack
}

// TestBase_Metrics_CloudWatchGrant asserts the cloudwatch:PutMetricData grant
// is emitted only when the exporter is selected, and is scoped to the
// effective namespace via the cloudwatch:namespace condition.
func TestBase_Metrics_CloudWatchGrant(t *testing.T) {

	t.Run("granted-when-cloudwatch-selected", func(t *testing.T) {
		boot := t20BaseBootstrap()
		boot.MetricsExporter = infra.MetricsExporterCloudWatch
		stack := metricsBuild(t, boot)
		tpl := assertions.Template_FromStack(stack, nil)

		if !t20CollectPolicyActions(tpl)["cloudwatch:PutMetricData"] {
			t.Fatalf("expected cloudwatch:PutMetricData grant when MetricsExporter=cloudwatch")
		}
		// The grant must be scoped to the effective namespace so a leaked
		// credential cannot publish to arbitrary namespaces.
		if !metricsHasNamespaceCondition(tpl, infra.DefaultMetricsNamespace) {
			t.Fatalf("cloudwatch:PutMetricData grant not scoped to namespace %q", infra.DefaultMetricsNamespace)
		}
	})

	t.Run("custom-namespace-scopes-condition", func(t *testing.T) {
		boot := t20BaseBootstrap()
		boot.MetricsExporter = infra.MetricsExporterCloudWatch
		boot.MetricsNamespace = "Acme/Bridge"
		stack := metricsBuild(t, boot)
		tpl := assertions.Template_FromStack(stack, nil)

		if !metricsHasNamespaceCondition(tpl, "Acme/Bridge") {
			t.Fatalf("cloudwatch grant not scoped to custom namespace")
		}
	})

	t.Run("absent-when-noop-default", func(t *testing.T) {
		stack := metricsBuild(t, t20BaseBootstrap()) // MetricsExporter unset => noop
		tpl := assertions.Template_FromStack(stack, nil)

		if t20CollectPolicyActions(tpl)["cloudwatch:PutMetricData"] {
			t.Fatalf("unexpected cloudwatch:PutMetricData grant for noop (unset) exporter")
		}
	})

	t.Run("absent-when-explicit-noop", func(t *testing.T) {
		boot := t20BaseBootstrap()
		boot.MetricsExporter = infra.MetricsExporterNoop
		stack := metricsBuild(t, boot)
		tpl := assertions.Template_FromStack(stack, nil)

		if t20CollectPolicyActions(tpl)["cloudwatch:PutMetricData"] {
			t.Fatalf("unexpected cloudwatch:PutMetricData grant for explicit noop exporter")
		}
	})
}

// TestBase_Metrics_EnvPlumbing asserts the exporter selection + namespace ride
// in the bootstrap JSON env var so the container actually receives them.
func TestBase_Metrics_EnvPlumbing(t *testing.T) {

	boot := t20BaseBootstrap()
	boot.MetricsExporter = infra.MetricsExporterCloudWatch
	boot.MetricsNamespace = "Acme/Bridge"
	boot.InstanceID = "control-0"
	stack := metricsBuild(t, boot, 2048)
	tpl := assertions.Template_FromStack(stack, nil)

	main := t20BaseMainContainer(t, t20BaseFindTaskDef(t, tpl))
	envs := main["Environment"].([]any)
	got := envFor(envs, "GOBRIDGE_FILEBASED_BOOTSTRAP_JSON")
	for _, want := range []string{
		`"metrics_exporter":"cloudwatch"`,
		`"metrics_namespace":"Acme/Bridge"`,
		`"instance_id":"control-0"`,
		`"container_memory_bytes":2147483648`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("bootstrap JSON env missing %s; got %s", want, got)
		}
	}
}

// metricsHasNamespaceCondition reports whether some IAM policy statement grants
// cloudwatch:PutMetricData with a StringEquals cloudwatch:namespace == ns.
func metricsHasNamespaceCondition(tpl assertions.Template, ns string) bool {
	policies := tpl.FindResources(jsii.String("AWS::IAM::Policy"), nil)
	for _, raw := range *policies {
		props, _ := (*raw)["Properties"].(map[string]any)
		doc, _ := props["PolicyDocument"].(map[string]any)
		stmts, _ := doc["Statement"].([]any)
		for _, st := range stmts {
			m, _ := st.(map[string]any)
			hasAction := false
			for _, a := range t20NormalizeActions(m["Action"]) {
				if a == "cloudwatch:PutMetricData" {
					hasAction = true
				}
			}
			if !hasAction {
				continue
			}
			cond, _ := m["Condition"].(map[string]any)
			eq, _ := cond["StringEquals"].(map[string]any)
			if eq["cloudwatch:namespace"] == ns {
				return true
			}
		}
	}
	return false
}
