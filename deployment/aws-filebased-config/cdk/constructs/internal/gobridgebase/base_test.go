//go:build !race

package gobridgebase_test

import (
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/assertions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/jsii-runtime-go"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/imgsource"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/internal/gobridgebase"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/internal/source"
)

func TestNew_Control_TaskDefHasOnlyMainContainer(t *testing.T) {
	stack, _ := newBuilt(t, gobridgebase.ModeControl, sampleYAML)
	tpl := assertions.Template_FromStack(stack, nil)

	tpl.ResourceCountIs(jsii.String("AWS::ECS::TaskDefinition"), jsii.Number(1))
	tds := tpl.FindResources(jsii.String("AWS::ECS::TaskDefinition"), nil)
	if tds == nil || len(*tds) != 1 {
		t.Fatalf("expected exactly 1 task def, got %v", tds)
	}
	for _, raw := range *tds {
		props := (*raw)["Properties"].(map[string]any)
		cds := props["ContainerDefinitions"].([]any)
		if len(cds) != 1 {
			t.Fatalf("expected 1 container, got %d", len(cds))
		}
		var names []string
		for _, cd := range cds {
			names = append(names, cd.(map[string]any)["Name"].(string))
		}
		if names[0] != "gobridge" {
			t.Fatalf("expected gobridge container, got %v", names)
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

func TestNew_MainHasNoInitContainerDependency(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode gobridgebase.Mode
	}{
		{"control", gobridgebase.ModeControl},
		{"worker", gobridgebase.ModeWorker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stack, _ := newBuilt(t, tc.mode, sampleYAML)
			if deps := mainContainer(t, stack)["DependsOn"]; deps != nil {
				t.Fatalf("main must not depend on an initialization container: %v", deps)
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
	tpl.ResourceCountIs(jsii.String("AWS::Logs::LogGroup"), jsii.Number(1))

	groups := tpl.FindResources(jsii.String("AWS::Logs::LogGroup"), nil)
	var gotMain bool
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
		if entry["DeletionPolicy"] != "Retain" || entry["UpdateReplacePolicy"] != "Retain" {
			t.Fatalf("log group %s default removal policy must be Retain (got %v / %v)",
				name, entry["DeletionPolicy"], entry["UpdateReplacePolicy"])
		}
	}
	if !gotMain {
		t.Fatal("expected the main log group")
	}
}

func TestNew_LogGroup_RemovalPolicyOverride(t *testing.T) {
	stack, vpc, efs := newScope(t)
	src := source.NewAsset(writeTempYAML(t, sampleYAML))
	gobridgebase.New(stack, jsii.String("Bridge"), &gobridgebase.Props{
		Mode:             gobridgebase.ModeControl,
		Vpc:              vpc,
		EfsConfig:        efs,
		Image:            imgsource.NewRegistry("gobridge@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
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

func TestNew_IAMStatementsPresentForEfs(t *testing.T) {
	stack, _ := newBuilt(t, gobridgebase.ModeControl, sampleYAML)
	tpl := assertions.Template_FromStack(stack, nil)

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
				Image:     imgsource.NewRegistry("gobridge@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
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
		Image:              imgsource.NewRegistry("gobridge@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Bootstrap:          bootstrap(),
		Source:             src,
		DisableHealthCheck: jsii.Bool(true),
	})
	m := mainContainer(t, stack)
	if _, ok := m["HealthCheck"]; ok {
		t.Fatalf("expected no HealthCheck when DisableHealthCheck=true, got %v", m["HealthCheck"])
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
