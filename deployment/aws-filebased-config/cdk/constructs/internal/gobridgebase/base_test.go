//go:build !race

package gobridgebase_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsecs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/jsii-runtime-go"

	cdkconstructs "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/gobridgebase"
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
		Image:     awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap: bootstrap(),
		Source:    src,
	})
	return stack, b
}

func TestNew_Control_TaskDefHasMainAndSeederContainers(t *testing.T) {
	stack, _ := newBuilt(t, gobridgebase.ModeControl, sampleYAML)
	tpl := assertions.Template_FromStack(stack, nil)

	tpl.ResourceCountIs(jsii.String("AWS::ECS::TaskDefinition"), jsii.Number(1))
	// 1 task def with 2 containers — assert by walking ContainerDefinitions.
	tds := tpl.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
	if tds == nil || len(*tds) != 1 {
		t.Fatalf("expected exactly 1 task def, got %v", tds)
	}
	for _, raw := range *tds {
		props := (*raw)["Properties"].(map[string]any)
		cds := props["ContainerDefinitions"].([]any)
		if len(cds) != 2 {
			t.Fatalf("expected 2 containers, got %d", len(cds))
		}
		var names []string
		for _, cd := range cds {
			names = append(names, cd.(map[string]any)["Name"].(string))
		}
		gotMain, gotSeeder := false, false
		for _, n := range names {
			if n == "gobridge" {
				gotMain = true
			}
			if n == "seeder" {
				gotSeeder = true
			}
		}
		if !gotMain || !gotSeeder {
			t.Fatalf("expected gobridge+seeder containers, got %v", names)
		}
	}
}

func TestNew_Control_MainMountIsRW_WorkerMountIsRO(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       gobridgebase.Mode
		wantMainRO bool
	}{
		{"control", gobridgebase.ModeControl, false},
		{"worker", gobridgebase.ModeWorker, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stack, _ := newBuilt(t, tc.mode, sampleYAML)
			tpl := assertions.Template_FromStack(stack, nil)
			tds := tpl.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
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
						t.Fatalf("want 1 mount point, got %d", len(mps))
					}
					ro, _ := mps[0].(map[string]any)["ReadOnly"].(bool)
					if ro != tc.wantMainRO {
						t.Fatalf("main ReadOnly = %v, want %v", ro, tc.wantMainRO)
					}
				}
			}
		})
	}
}

func TestNew_Seeder_EnvAndDependency(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     gobridgebase.Mode
		wantMode string
	}{
		{"control", gobridgebase.ModeControl, "SeedOnce"},
		{"worker", gobridgebase.ModeWorker, "AdoptValid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stack, _ := newBuilt(t, tc.mode, sampleYAML)
			tpl := assertions.Template_FromStack(stack, nil)
			tds := tpl.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
			for _, raw := range *tds {
				props := (*raw)["Properties"].(map[string]any)
				cds := props["ContainerDefinitions"].([]any)
				for _, cd := range cds {
					m := cd.(map[string]any)
					switch m["Name"] {
					case "seeder":
						if ess, _ := m["Essential"].(bool); ess {
							t.Fatalf("seeder must be Essential=false")
						}
						envs := m["Environment"].([]any)
						gotMode := envFor(envs, "MODE")
						if gotMode != tc.wantMode {
							t.Fatalf("MODE = %q, want %q", gotMode, tc.wantMode)
						}
						if envFor(envs, "EXPECTED_HASH") == "" {
							t.Fatalf("EXPECTED_HASH must be non-empty")
						}
						if envFor(envs, "EFS_TARGET_PATH") != "/var/lib/gobridge/bridge.yaml" {
							t.Fatalf("EFS_TARGET_PATH wrong: %q", envFor(envs, "EFS_TARGET_PATH"))
						}
					case "gobridge":
						deps, _ := m["DependsOn"].([]any)
						if len(deps) != 1 {
							t.Fatalf("main must depend on 1 container, got %v", deps)
						}
						dep := deps[0].(map[string]any)
						if dep["Condition"] != "SUCCESS" || dep["ContainerName"] != "seeder" {
							t.Fatalf("dep wrong: %v", dep)
						}
					}
				}
			}
		})
	}
}

// envFor scans an ECS Environment array (each entry is {Name,Value})
// and returns the Value for Name. Value may be a CFn intrinsic (map)
// — in that case we return a JSON-ish string so tests assert "non-empty".
func envFor(envs []any, name string) string {
	for _, e := range envs {
		m := e.(map[string]any)
		if m["Name"] != name {
			continue
		}
		switch v := m["Value"].(type) {
		case string:
			return v
		case nil:
			return ""
		default:
			return "<intrinsic>"
		}
	}
	return ""
}

func TestNew_PortMappings_FromBootstrapDefaults(t *testing.T) {
	// Without HTTP receivers in yaml, transport HTTP port is omitted;
	// admin + monitor come from bootstrap defaults.
	_, b := newBuilt(t, gobridgebase.ModeControl, sampleYAML)
	ports := mapPorts(b.PortMappings)
	if !ports[8080] {
		t.Fatalf("missing admin 8080: %v", ports)
	}
	if !ports[8081] {
		t.Fatalf("missing monitor 8081: %v", ports)
	}
	if ports[8082] {
		t.Fatalf("transport 8082 must be absent without http receiver: %v", ports)
	}
}

func mapPorts(p []gobridgebase.PortMapping) map[int]bool {
	out := map[int]bool{}
	for _, m := range p {
		out[int(m.Port)] = true
	}
	return out
}

func TestNew_LogGroup_PrefixAndDefaultRetainPolicy(t *testing.T) {
	stack, _ := newBuilt(t, gobridgebase.ModeControl, sampleYAML)
	tpl := assertions.Template_FromStack(stack, nil)
	tpl.ResourceCountIs(jsii.String("AWS::Logs::LogGroup"), jsii.Number(2))

	groups := tpl.FindResources(jsii.String("AWS::Logs::LogGroup"), nil)
	var gotMain, gotSeeder bool
	for _, raw := range *groups {
		entry := (*raw)
		props := entry["Properties"].(map[string]any)
		name := props["LogGroupName"].(string)
		if !strings.HasPrefix(name, "/gobridge/TestStack/Bridge/") {
			t.Fatalf("log group name does not match prefix scheme: %s", name)
		}
		if strings.HasSuffix(name, "/gobridge") {
			gotMain = true
		}
		if strings.HasSuffix(name, "/seeder") {
			gotSeeder = true
		}
		if entry["DeletionPolicy"] != "Retain" || entry["UpdateReplacePolicy"] != "Retain" {
			t.Fatalf("log group %s default removal policy must be Retain (got %v / %v)",
				name, entry["DeletionPolicy"], entry["UpdateReplacePolicy"])
		}
	}
	if !gotMain || !gotSeeder {
		t.Fatalf("expected main+seeder log groups, got main=%v seeder=%v", gotMain, gotSeeder)
	}
}

func TestNew_LogGroup_RemovalPolicyOverride(t *testing.T) {
	stack, vpc, efs := newScope(t)
	src := source.NewAsset(writeTempYAML(t, sampleYAML))
	gobridgebase.New(stack, jsii.String("Bridge"), &gobridgebase.Props{
		Mode:             gobridgebase.ModeControl,
		Vpc:              vpc,
		EfsConfig:        efs,
		Image:            awsecs.ContainerImage_FromRegistry(jsii.String("gobridge:latest"), nil),
		Bootstrap:        bootstrap(),
		Source:           src,
		LogRemovalPolicy: awscdk.RemovalPolicy_DESTROY,
		LogRetention:     awslogs.RetentionDays_ONE_WEEK,
	})
	tpl := assertions.Template_FromStack(stack, nil)
	groups := tpl.FindResources(jsii.String("AWS::Logs::LogGroup"), nil)
	for _, raw := range *groups {
		entry := (*raw)
		if entry["DeletionPolicy"] != "Delete" {
			t.Fatalf("expected DeletionPolicy=Delete after override, got %v", entry["DeletionPolicy"])
		}
	}
}

func TestNew_IAMStatementsPresentForEfsAndS3Asset(t *testing.T) {
	stack, _ := newBuilt(t, gobridgebase.ModeControl, sampleYAML)
	tpl := assertions.Template_FromStack(stack, nil)

	// Asset GrantRead on the task role becomes an s3:GetObject statement.
	tpl.HasResourceProperties(jsii.String("AWS::IAM::Policy"), map[string]any{
		"PolicyDocument": assertions.Match_ObjectLike(&map[string]any{
			"Statement": assertions.Match_ArrayWith(&[]any{
				assertions.Match_ObjectLike(&map[string]any{
					"Action": assertions.Match_ArrayWith(&[]any{"s3:GetObject*"}),
				}),
			}),
		}),
	})

	// EFS ClientMount + ClientWrite for control mode.
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

func TestNew_Worker_EFSGrantOmitsClientWrite(t *testing.T) {
	stack, _ := newBuilt(t, gobridgebase.ModeWorker, sampleYAML)
	tpl := assertions.Template_FromStack(stack, nil)

	policies := tpl.FindResources(jsii.String("AWS::IAM::Policy"), nil)
	for _, raw := range *policies {
		props := (*raw)["Properties"].(map[string]any)
		doc := props["PolicyDocument"].(map[string]any)
		stmts := doc["Statement"].([]any)
		for _, s := range stmts {
			actionField := s.(map[string]any)["Action"]
			actions := normalizeActions(actionField)
			for _, a := range actions {
				if a == "elasticfilesystem:ClientWrite" {
					t.Fatalf("worker task role must NOT have ClientWrite, found in %v", actions)
				}
			}
		}
	}
}

func normalizeActions(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func TestNew_PanicsOnInvalidProps(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(p *gobridgebase.Props)
		wantSub string
	}{
		{"bad-mode", func(p *gobridgebase.Props) { p.Mode = "what" }, "Mode"},
		{"missing-vpc", func(p *gobridgebase.Props) { p.Vpc = nil }, "Vpc"},
		{"missing-efs", func(p *gobridgebase.Props) { p.EfsConfig = nil }, "EfsConfig"},
		{"missing-image", func(p *gobridgebase.Props) { p.Image = nil }, "Image"},
		{"missing-source", func(p *gobridgebase.Props) { p.Source = nil }, "Source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stack, vpc, efs := newScope(t)
			src := source.NewAsset(writeTempYAML(t, sampleYAML))
			p := &gobridgebase.Props{
				Mode:      gobridgebase.ModeControl,
				Vpc:       vpc,
				EfsConfig: efs,
				Image:     awsecs.ContainerImage_FromRegistry(jsii.String("img:latest"), nil),
				Bootstrap: bootstrap(),
				Source:    src,
			}
			tc.mutate(p)
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("expected panic")
				}
				if !strings.Contains(asString(r), tc.wantSub) {
					t.Fatalf("panic %q does not mention %q", asString(r), tc.wantSub)
				}
			}()
			gobridgebase.New(stack, jsii.String("X"), p)
		})
	}
}

func mainContainer(t *testing.T, stack awscdk.Stack) map[string]any {
	t.Helper()
	tpl := assertions.Template_FromStack(stack, nil)
	tds := tpl.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
	for _, raw := range *tds {
		props := (*raw)["Properties"].(map[string]any)
		for _, cd := range props["ContainerDefinitions"].([]any) {
			m := cd.(map[string]any)
			if m["Name"] == "gobridge" {
				return m
			}
		}
	}
	t.Fatal("main (gobridge) container not found")
	return nil
}

// TestNew_Main_HealthCheckStopTimeoutAndUser asserts the terminal-runtime
// backstop wiring: a container HealthCheck that runs the static binary against
// the monitor /live endpoint, a StopTimeout longer than the drain budget, and
// a non-root User.
func TestNew_Main_HealthCheckStopTimeoutAndUser(t *testing.T) {
	stack, _ := newBuilt(t, gobridgebase.ModeControl, sampleYAML)
	m := mainContainer(t, stack)

	// StopTimeout must exceed Fargate's 30s default drain budget.
	if got, _ := m["StopTimeout"].(float64); got < 45 {
		t.Fatalf("StopTimeout = %v, want >= 45 (drain budget + margin)", m["StopTimeout"])
	}

	// Non-root user.
	if got, _ := m["User"].(string); got != "65532:65532" {
		t.Fatalf("User = %q, want %q", got, "65532:65532")
	}

	// HealthCheck reuses the binary (no curl/wget in the distroless image).
	hc, ok := m["HealthCheck"].(map[string]any)
	if !ok {
		t.Fatalf("main container has no HealthCheck: %v", m["HealthCheck"])
	}
	cmd, _ := hc["Command"].([]any)
	var parts []string
	for _, c := range cmd {
		parts = append(parts, c.(string))
	}
	joined := strings.Join(parts, " ")
	if !strings.Contains(joined, "-healthcheck") || !strings.Contains(joined, "gobridge-filebased") {
		t.Fatalf("HealthCheck.Command = %v, want binary + -healthcheck", parts)
	}
}

// TestNew_Main_HealthCheckDisabled verifies DisableHealthCheck removes the
// probe (for the ALB-target-health-check case).
func TestNew_Main_HealthCheckDisabled(t *testing.T) {
	stack, vpc, efs := newScope(t)
	src := source.NewAsset(writeTempYAML(t, sampleYAML))
	gobridgebase.New(stack, jsii.String("X"), &gobridgebase.Props{
		Mode:               gobridgebase.ModeControl,
		Vpc:                vpc,
		EfsConfig:          efs,
		Image:              awsecs.ContainerImage_FromRegistry(jsii.String("img:latest"), nil),
		Bootstrap:          bootstrap(),
		Source:             src,
		DisableHealthCheck: jsii.Bool(true),
	})
	m := mainContainer(t, stack)
	if _, ok := m["HealthCheck"]; ok {
		t.Fatalf("expected no HealthCheck when DisableHealthCheck=true, got %v", m["HealthCheck"])
	}
}

// TestNew_PanicsOnPlaceholderSeederDigest verifies the synth-time guard: a
// SeederImage override pinned to the all-zeros placeholder digest (or an
// unpinned ref) must panic rather than synth a dead-on-arrival task whose main
// container waits forever on a seeder that can never be pulled.
func TestNew_PanicsOnPlaceholderSeederDigest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ref     string
		wantSub string
	}{
		{
			name:    "all-zeros",
			ref:     "public.ecr.aws/aws-cli/aws-cli:2@sha256:" + strings.Repeat("0", 64),
			wantSub: "all-zeros",
		},
		{
			name:    "unpinned",
			ref:     "public.ecr.aws/aws-cli/aws-cli:2",
			wantSub: "fully-pinned",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stack, vpc, efs := newScope(t)
			src := source.NewAsset(writeTempYAML(t, sampleYAML))
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("expected panic on invalid seeder digest")
				}
				if !strings.Contains(asString(r), tc.wantSub) {
					t.Fatalf("panic %q does not mention %q", asString(r), tc.wantSub)
				}
			}()
			gobridgebase.New(stack, jsii.String("X"), &gobridgebase.Props{
				Mode:        gobridgebase.ModeControl,
				Vpc:         vpc,
				EfsConfig:   efs,
				Image:       awsecs.ContainerImage_FromRegistry(jsii.String("img:latest"), nil),
				Bootstrap:   bootstrap(),
				Source:      src,
				SeederImage: jsii.String(tc.ref),
			})
		})
	}
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case error:
		return t.Error()
	default:
		return ""
	}
}
