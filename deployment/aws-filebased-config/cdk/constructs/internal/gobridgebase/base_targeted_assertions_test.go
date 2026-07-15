//go:build !race

package gobridgebase_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/jsii-runtime-go"

	// Register the http transport plugin so yaml parsing of
	// "transport: http" succeeds for the port-mappings fixture.
	_ "github.com/mariotoffia/gobridge/adapters/http/transport"

	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/gobridgebase"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
)

const t20BaseSampleYAML = `
bridge:
  id: test-bridge
`

const t20BaseHTTPYAML = `
bridge:
  id: test-bridge
http:
  admin_addr: ":8080"
receivers:
  - id: webhook
    transport: http
    options:
      path: /hooks/webhook
`

func t20BaseWriteYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return p
}

func t20BaseBootstrap() infra.BootstrapConfig {
	return infra.BootstrapConfig{
		BridgeID:         "bridge-1",
		ConfigFilePath:   "/var/lib/gobridge/bridge.yaml",
		AdminAPIKeyParam: "/test/admin",
	}
}

func t20BaseBuild(t *testing.T, mode gobridgebase.Mode, yaml string) (awscdk.Stack, *gobridgebase.Built) {
	t.Helper()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("S"), nil)
	vpc := awsec2.NewVpc(stack, jsii.String("Vpc"), nil)
	efs := cdkconstructs.NewGoBridgeEfsConfig(stack, jsii.String("Efs"),
		&cdkconstructs.GoBridgeEfsConfigProps{Vpc: vpc})
	src := source.NewAsset(t20BaseWriteYAML(t, yaml))
	b := gobridgebase.New(stack, jsii.String("Bridge"), &gobridgebase.Props{
		Mode:      mode,
		Vpc:       vpc,
		EfsConfig: efs,
		Image:     awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap: t20BaseBootstrap(),
		Source:    src,
	})
	return stack, b
}

func t20BaseFindTaskDef(t *testing.T, tpl assertions.Template) map[string]any {
	t.Helper()
	tds := tpl.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
	if tds == nil || len(*tds) != 1 {
		t.Fatalf("want 1 TaskDefinition, got %v", tds)
	}
	for _, raw := range *tds {
		return *raw
	}
	return nil
}

func t20BaseMainContainer(t *testing.T, td map[string]any) map[string]any {
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

// Test_T20_Base_TaskDef_FargateBaselineShape asserts the structural baseline
// of the emitted ECS TaskDefinition: NetworkMode=awsvpc, Cpu/Memory match the
// bootstrap defaults, FARGATE compatibility, ExecutionRole present, and the
// main container's LogConfiguration is awslogs with the expected stream
// prefix.
func Test_T20_Base_TaskDef_FargateBaselineShape(t *testing.T) {
	defer jsii.Close()
	stack, _ := t20BaseBuild(t, gobridgebase.ModeControl, t20BaseSampleYAML)
	tpl := assertions.Template_FromStack(stack, nil)

	tpl.HasResourceProperties(jsii.String("AWS::ECS::TaskDefinition"), map[string]any{
		"NetworkMode": "awsvpc",
		"RequiresCompatibilities": assertions.Match_ArrayWith(&[]any{
			"FARGATE",
		}),
		"ExecutionRoleArn": assertions.Match_AnyValue(),
		"TaskRoleArn":      assertions.Match_AnyValue(),
	})

	td := t20BaseFindTaskDef(t, tpl)
	main := t20BaseMainContainer(t, td)
	logCfg, _ := main["LogConfiguration"].(map[string]any)
	if logCfg == nil {
		t.Fatalf("main container missing LogConfiguration")
	}
	if drv, _ := logCfg["LogDriver"].(string); drv != "awslogs" {
		t.Fatalf("LogDriver = %q, want awslogs", drv)
	}
	opts, _ := logCfg["Options"].(map[string]any)
	if pfx, _ := opts["awslogs-stream-prefix"].(string); pfx == "" {
		t.Fatalf("awslogs-stream-prefix must be non-empty, got opts=%v", opts)
	}
}

// Test_T20_Base_PortMappings_AdminPlusHTTPReceiver covers the port-mapping
// dimension required by T20: the yaml fixture declares an HTTP receiver
// (transport=http on port 9000-equivalent default 8082) plus admin defaults
// to 8080. The TaskDefinition must surface BOTH PortMappings entries with
// ContainerPort + Protocol=tcp.
//
// Note: bootstrap default for transport HTTP is 8082, not 9000 — there is no
// production knob to override to 9000 from the construct surface today. The
// invariant we assert (admin + transport both on tcp) is the contract; the
// exact numeric for transport defaults to the bootstrap-defined 8082.
func Test_T20_Base_PortMappings_AdminPlusHTTPReceiver(t *testing.T) {
	defer jsii.Close()
	stack, _ := t20BaseBuild(t, gobridgebase.ModeControl, t20BaseHTTPYAML)
	tpl := assertions.Template_FromStack(stack, nil)
	td := t20BaseFindTaskDef(t, tpl)
	main := t20BaseMainContainer(t, td)
	pms, _ := main["PortMappings"].([]any)
	if len(pms) < 2 {
		t.Fatalf("expected at least 2 PortMappings (admin + transport), got %d: %v", len(pms), pms)
	}
	got := map[float64]string{}
	for _, pm := range pms {
		m := pm.(map[string]any)
		port, _ := m["ContainerPort"].(float64)
		proto, _ := m["Protocol"].(string)
		got[port] = proto
	}
	for _, want := range []float64{8080, 8082} {
		proto, ok := got[want]
		if !ok {
			t.Fatalf("missing PortMapping for %.0f, got %v", want, got)
		}
		if proto != "tcp" {
			t.Fatalf("PortMapping %.0f Protocol = %q, want tcp", want, proto)
		}
	}
}

// Test_T20_Base_IAM_SeederAssetReadGrants asserts the seeder asset task role
// policy includes the S3 statements emitted by Asset.GrantRead — namely
// s3:GetObject* and s3:GetBucket* (the latter covers GetBucketLocation).
func Test_T20_Base_IAM_SeederAssetReadGrants(t *testing.T) {
	defer jsii.Close()
	stack, _ := t20BaseBuild(t, gobridgebase.ModeControl, t20BaseSampleYAML)
	tpl := assertions.Template_FromStack(stack, nil)

	policies := tpl.FindResources(jsii.String("AWS::IAM::Policy"), nil)
	var sawGetObject, sawGetBucket bool
	for _, raw := range *policies {
		props := (*raw)["Properties"].(map[string]any)
		doc, _ := props["PolicyDocument"].(map[string]any)
		stmts, _ := doc["Statement"].([]any)
		for _, s := range stmts {
			actionField := s.(map[string]any)["Action"]
			for _, a := range t20NormalizeActions(actionField) {
				if strings.HasPrefix(a, "s3:GetObject") {
					sawGetObject = true
				}
				if strings.HasPrefix(a, "s3:GetBucket") {
					sawGetBucket = true
				}
			}
		}
	}
	if !sawGetObject {
		t.Fatalf("no s3:GetObject* statement found on any IAM policy")
	}
	if !sawGetBucket {
		t.Fatalf("no s3:GetBucket* statement found on any IAM policy " +
			"(GetBucketLocation is part of Asset.GrantRead's bucket grants)")
	}
}

// Test_T20_Base_IAM_EFSPerMode asserts the per-mode EFS grant matrix:
// control role policies include BOTH ClientMount and ClientWrite; worker
// role policies include ClientMount but NEVER ClientWrite (defense in
// depth complement to the RO mount).
func Test_T20_Base_IAM_EFSPerMode(t *testing.T) {
	defer jsii.Close()
	for _, tc := range []struct {
		name           string
		mode           gobridgebase.Mode
		mustHaveWrite  bool
		mustHaveMount  bool
		mustNeverWrite bool
	}{
		{"control", gobridgebase.ModeControl, true, true, false},
		{"worker", gobridgebase.ModeWorker, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stack, _ := t20BaseBuild(t, tc.mode, t20BaseSampleYAML)
			tpl := assertions.Template_FromStack(stack, nil)
			policies := tpl.FindResources(jsii.String("AWS::IAM::Policy"), nil)

			var sawMount, sawWrite bool
			for _, raw := range *policies {
				props := (*raw)["Properties"].(map[string]any)
				doc, _ := props["PolicyDocument"].(map[string]any)
				stmts, _ := doc["Statement"].([]any)
				for _, s := range stmts {
					for _, a := range t20NormalizeActions(s.(map[string]any)["Action"]) {
						if a == "elasticfilesystem:ClientMount" {
							sawMount = true
						}
						if a == "elasticfilesystem:ClientWrite" {
							sawWrite = true
						}
					}
				}
			}
			if tc.mustHaveMount && !sawMount {
				t.Fatalf("%s: ClientMount missing", tc.name)
			}
			if tc.mustHaveWrite && !sawWrite {
				t.Fatalf("%s: ClientWrite missing on control role", tc.name)
			}
			if tc.mustNeverWrite && sawWrite {
				t.Fatalf("%s: worker role MUST NOT include ClientWrite", tc.name)
			}
		})
	}
}

// Test_T20_Base_Mounts_PerModeReadOnlyFlag asserts MountPoints[0].ReadOnly is
// false for control and true for worker on the gobridge main container.
// (Existing TestNew_Control_MainMountIsRW_WorkerMountIsRO covers this; the
// duplicate here is intentional per T20 spec which requires per-construct
// mount-readOnly assertions in this targeted file.)
func Test_T20_Base_Mounts_PerModeReadOnlyFlag(t *testing.T) {
	defer jsii.Close()
	for _, tc := range []struct {
		name string
		mode gobridgebase.Mode
		want bool
	}{
		{"control-rw", gobridgebase.ModeControl, false},
		{"worker-ro", gobridgebase.ModeWorker, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stack, _ := t20BaseBuild(t, tc.mode, t20BaseSampleYAML)
			tpl := assertions.Template_FromStack(stack, nil)
			td := t20BaseFindTaskDef(t, tpl)
			main := t20BaseMainContainer(t, td)
			mps := main["MountPoints"].([]any)
			if len(mps) != 1 {
				t.Fatalf("want 1 mount, got %d", len(mps))
			}
			ro, _ := mps[0].(map[string]any)["ReadOnly"].(bool)
			if ro != tc.want {
				t.Fatalf("ReadOnly = %v, want %v", ro, tc.want)
			}
		})
	}
}

const t20BaseDynamoStoreYAML = `
bridge:
  id: test-bridge
stores:
  outbox:
    type: dynamodb
    options:
      table_name: bridge-outbox-tbl
`

const t20BaseDefaultDynamoStoreYAML = `
bridge:
  id: test-bridge
stores:
  outbox:
    type: dynamodb
`

func Test_T20_Base_IAM_DynamoDBStoreGrantUsesRuntimeDefaultTable(t *testing.T) {
	defer jsii.Close()
	stack, _ := t20BaseBuild(t, gobridgebase.ModeControl, t20BaseDefaultDynamoStoreYAML)
	assembly := awscdk.App_Of(stack).Synth(nil)
	rendered := assembly.GetStackByName(stack.StackName()).Template()
	renderedJSON, err := json.Marshal(rendered)
	if err != nil {
		t.Fatalf("marshal synthesized template: %v", err)
	}
	if !strings.Contains(string(renderedJSON), "gobridge-outbox") {
		t.Fatalf("synthesized grants do not reference runtime default table: %s", renderedJSON)
	}
}

const t20BaseDefaultLeaseStoreYAML = `
bridge:
  id: test-bridge
stores:
  lease:
    type: dynamodb
`

func Test_T20_Base_IAM_DefaultLeaseTableGetsTTLPreflightGrant(t *testing.T) {
	defer jsii.Close()
	stack, _ := t20BaseBuild(t, gobridgebase.ModeControl, t20BaseDefaultLeaseStoreYAML)
	assembly := awscdk.App_Of(stack).Synth(nil)
	rendered, err := json.Marshal(assembly.GetStackByName(stack.StackName()).Template())
	if err != nil {
		t.Fatalf("marshal synthesized template: %v", err)
	}
	if !strings.Contains(string(rendered), "gobridge-leases") {
		t.Fatalf("lease grant does not reference the runtime default table: %s", rendered)
	}
	if !t20CollectPolicyActions(assertions.Template_FromStack(stack, nil))["dynamodb:DescribeTimeToLive"] {
		t.Fatal("default lease table is missing dynamodb:DescribeTimeToLive")
	}
}

// Test_T20_Base_IAM_DynamoDBStoreGrant asserts that a bridge config
// referencing a DynamoDB-backed store with an explicit table_name emits
// DynamoDB read/write IAM actions on the task role, and that a config
// with no DynamoDB store emits none. Guards J4: the AWS profile must
// grant the table actions the DynamoDB store adapter performs.
func Test_T20_Base_IAM_DynamoDBStoreGrant(t *testing.T) {
	defer jsii.Close()

	t.Run("granted-with-table-name", func(t *testing.T) {
		stack, _ := t20BaseBuild(t, gobridgebase.ModeControl, t20BaseDynamoStoreYAML)
		tpl := assertions.Template_FromStack(stack, nil)
		actions := t20CollectPolicyActions(tpl)
		for _, want := range []string{
			"dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:UpdateItem",
			"dynamodb:Query", "dynamodb:TransactWriteItems", "dynamodb:DescribeTable",
		} {
			if !actions[want] {
				t.Fatalf("missing IAM action %q for DynamoDB store (got %v)", want, keysOf(actions))
			}
		}
		for _, forbidden := range []string{"dynamodb:DeleteItem", "dynamodb:Scan", "dynamodb:CreateTable", "dynamodb:UpdateTable", "dynamodb:DeleteTable", "dynamodb:UpdateTimeToLive"} {
			if actions[forbidden] {
				t.Fatalf("unexpected IAM action %q for outbox store", forbidden)
			}
		}
	})

	t.Run("absent-without-dynamo-store", func(t *testing.T) {
		stack, _ := t20BaseBuild(t, gobridgebase.ModeControl, t20BaseSampleYAML)
		tpl := assertions.Template_FromStack(stack, nil)
		actions := t20CollectPolicyActions(tpl)
		if actions["dynamodb:PutItem"] {
			t.Fatalf("unexpected dynamodb:PutItem action when no DynamoDB store configured")
		}
	})
}

// t20CollectPolicyActions gathers every IAM action string across all
// AWS::IAM::Policy statements in the synthesized template.
func t20CollectPolicyActions(tpl assertions.Template) map[string]bool {
	out := map[string]bool{}
	policies := tpl.FindResources(jsii.String("AWS::IAM::Policy"), nil)
	for _, raw := range *policies {
		props, _ := (*raw)["Properties"].(map[string]any)
		doc, _ := props["PolicyDocument"].(map[string]any)
		stmts, _ := doc["Statement"].([]any)
		for _, st := range stmts {
			m, _ := st.(map[string]any)
			for _, a := range t20NormalizeActions(m["Action"]) {
				out[a] = true
			}
		}
	}
	return out
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func t20NormalizeActions(v any) []string {
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
