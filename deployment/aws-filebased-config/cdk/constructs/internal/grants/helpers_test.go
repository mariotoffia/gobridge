//go:build !race

package grants_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/jsii-runtime-go"
)

// newTestStack builds a fresh CDK stack with a no-op IAM role suitable
// for receiving grants. Each test must defer jsii.Close.
func newTestStack(t *testing.T) (awscdk.Stack, awsiam.Role) {
	t.Helper()
	app := awscdk.NewApp(nil)
	stack := awscdk.NewStack(app, jsii.String("Test"), nil)
	role := awsiam.NewRole(stack, jsii.String("Role"), &awsiam.RoleProps{
		AssumedBy: awsiam.NewServicePrincipal(jsii.String("ecs-tasks.amazonaws.com"), nil),
	})
	return stack, role
}

// collectAllowActions walks every AWS::IAM::Policy and AWS::IAM::Role
// resource in stack and returns the union of every Action string under
// every Effect=Allow statement. Action may be a single string or an
// array — both are flattened.
func collectAllowActions(t *testing.T, stack awscdk.Stack) map[string]bool {
	t.Helper()
	app := awscdk.App_Of(stack)
	asm := app.Synth(nil)
	tmpl := asm.GetStackByName(stack.StackName()).Template()
	raw, err := json.Marshal(tmpl)
	if err != nil {
		t.Fatalf("marshal template: %v", err)
	}
	var doc struct {
		Resources map[string]struct {
			Type       string         `json:"Type"`
			Properties map[string]any `json:"Properties"`
		} `json:"Resources"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal template: %v", err)
	}
	out := map[string]bool{}
	for _, r := range doc.Resources {
		if r.Type != "AWS::IAM::Policy" && r.Type != "AWS::IAM::Role" {
			continue
		}
		pd, ok := r.Properties["PolicyDocument"].(map[string]any)
		if !ok {
			// AWS::IAM::Role may have multiple Policies inline.
			if pols, ok := r.Properties["Policies"].([]any); ok {
				for _, p := range pols {
					if pm, ok := p.(map[string]any); ok {
						if d, ok := pm["PolicyDocument"].(map[string]any); ok {
							harvest(d, out)
						}
					}
				}
			}
			continue
		}
		harvest(pd, out)
	}
	return out
}

func harvest(pd map[string]any, out map[string]bool) {
	stmts, _ := pd["Statement"].([]any)
	for _, s := range stmts {
		sm, _ := s.(map[string]any)
		if sm["Effect"] != "Allow" {
			continue
		}
		switch a := sm["Action"].(type) {
		case string:
			out[a] = true
		case []any:
			for _, x := range a {
				if str, ok := x.(string); ok {
					out[str] = true
				}
			}
		}
	}
}

func mustHave(t *testing.T, actions map[string]bool, want ...string) {
	t.Helper()
	for _, w := range want {
		if !actions[w] {
			t.Errorf("missing action %q in policy; got %v", w, keys(actions))
		}
	}
}

func mustNotHave(t *testing.T, actions map[string]bool, deny ...string) {
	t.Helper()
	for _, d := range deny {
		if actions[d] {
			t.Errorf("unexpected action %q in policy", d)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// findAllowStatement walks every IAM policy in stack and returns the
// first Allow statement whose Action set contains any of wantActions.
// Returns nil if none found.
func findAllowStatement(t *testing.T, stack awscdk.Stack, wantActions ...string) map[string]any {
	t.Helper()
	app := awscdk.App_Of(stack)
	asm := app.Synth(nil)
	tmpl := asm.GetStackByName(stack.StackName()).Template()
	raw, err := json.Marshal(tmpl)
	if err != nil {
		t.Fatalf("marshal template: %v", err)
	}
	var doc struct {
		Resources map[string]struct {
			Type       string         `json:"Type"`
			Properties map[string]any `json:"Properties"`
		} `json:"Resources"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal template: %v", err)
	}
	want := map[string]bool{}
	for _, a := range wantActions {
		want[a] = true
	}
	var visit func(pd map[string]any) map[string]any //nolint:staticcheck // S1021 false positive: recursive closure must be declared before assignment.
	visit = func(pd map[string]any) map[string]any {
		stmts, _ := pd["Statement"].([]any)
		for _, s := range stmts {
			sm, _ := s.(map[string]any)
			if sm["Effect"] != "Allow" {
				continue
			}
			match := false
			switch a := sm["Action"].(type) {
			case string:
				if want[a] {
					match = true
				}
			case []any:
				for _, x := range a {
					if str, ok := x.(string); ok && want[str] {
						match = true
						break
					}
				}
			}
			if match {
				return sm
			}
		}
		return nil
	}
	for _, r := range doc.Resources {
		if r.Type != "AWS::IAM::Policy" && r.Type != "AWS::IAM::Role" {
			continue
		}
		if pd, ok := r.Properties["PolicyDocument"].(map[string]any); ok {
			if s := visit(pd); s != nil {
				return s
			}
		}
		if pols, ok := r.Properties["Policies"].([]any); ok {
			for _, p := range pols {
				if pm, ok := p.(map[string]any); ok {
					if d, ok := pm["PolicyDocument"].(map[string]any); ok {
						if s := visit(d); s != nil {
							return s
						}
					}
				}
			}
		}
	}
	return nil
}

// resourceContains returns true if the Resource field of an IAM
// statement (string, list, or CFN intrinsic map) equals or contains
// want. CFN intrinsics (Fn::Join, Fn::GetAtt) are matched by JSON
// substring since the rendered ARN is not yet a literal at synth time.
func resourceContains(resource any, want string) bool {
	switch v := resource.(type) {
	case string:
		return v == want || strings.Contains(v, want)
	case []any:
		for _, x := range v {
			if resourceContains(x, want) {
				return true
			}
		}
	case map[string]any:
		b, _ := json.Marshal(v)
		return strings.Contains(string(b), want)
	}
	return false
}
