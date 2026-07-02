package validate_test

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/validate"
)

func validRouteWithResolver() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test"},
		Receivers: []ports.ReceiverDef{
			{ID: "rx1", Transport: "http"},
		},
		Senders: []ports.SenderDef{
			{ID: "tx1", Transport: "http"},
			{ID: "tx2", Transport: "http"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "tx1", Address: "/events/a"},
			{ID: "b2", SenderID: "tx2", Address: "/events/b"},
		},
		Routes: []ports.RouteDef{
			{
				ID:         "r1",
				ReceiverID: "rx1",
				Bindings:   []string{"b1", "b2"},
			},
		},
	}
}

// errorString runs ValidateBlueprintGraph and returns a flattened
// error string. It returns "" when the result has no errors (warnings
// are ignored — direct_hold always emits a warning).
func errorString(t *testing.T, cfg *ports.BridgeConfig) string {
	t.Helper()
	res := validate.ValidateBlueprintGraph(cfg)
	if res == nil || !res.HasErrors() {
		return ""
	}
	return res.Error()
}

func TestValidateBlueprintGraph_RulesType_Valid(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ports.ResolverDef{
		Type:           "rules",
		DefaultBinding: "b2",
		Rules: []ports.RuleDef{
			{BindingID: "b1", Match: []ports.ConditionDef{
				{Field: "subject", Operator: "prefix", Value: "orders."},
			}},
		},
	}
	if got := errorString(t, cfg); got != "" {
		t.Fatalf("expected no errors, got: %s", got)
	}
}

func TestValidateBlueprintGraph_InvalidResolverType(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ports.ResolverDef{Type: "unknown"}
	got := errorString(t, cfg)
	if got == "" {
		t.Fatal("expected error for invalid resolver type")
	}
	if !strings.Contains(got, "resolver.type") {
		t.Fatalf("expected resolver.type error, got: %s", got)
	}
}

func TestValidateBlueprintGraph_DefaultBindingNotInRoute(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ports.ResolverDef{
		Type:           "rules",
		DefaultBinding: "nonexistent",
		Rules:          []ports.RuleDef{{BindingID: "b1"}},
	}
	got := errorString(t, cfg)
	if got == "" {
		t.Fatal("expected error for unknown default_binding")
	}
	if !strings.Contains(got, "default_binding") {
		t.Fatalf("expected default_binding error, got: %s", got)
	}
}

func TestValidateBlueprintGraph_RuleBindingNotInRoute(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ports.ResolverDef{
		Type: "rules",
		Rules: []ports.RuleDef{
			{BindingID: "nonexistent", Match: []ports.ConditionDef{
				{Field: "subject", Operator: "eq", Value: "x"},
			}},
		},
	}
	got := errorString(t, cfg)
	if got == "" {
		t.Fatal("expected error for rule referencing unknown binding")
	}
	if !strings.Contains(got, "nonexistent") {
		t.Fatalf("expected binding ID in error, got: %s", got)
	}
}

func TestValidateBlueprintGraph_HeaderMap_Valid(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ports.ResolverDef{
		Type:      "header_map",
		HeaderKey: "x-tenant",
		HeaderMap: map[string]string{"acme": "b1", "globex": "b2"},
	}
	if got := errorString(t, cfg); got != "" {
		t.Fatalf("expected no errors, got: %s", got)
	}
}

func TestValidateBlueprintGraph_HeaderMap_MissingKey(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ports.ResolverDef{
		Type:      "header_map",
		HeaderMap: map[string]string{"acme": "b1"},
	}
	got := errorString(t, cfg)
	if got == "" {
		t.Fatal("expected error for missing header_key")
	}
	if !strings.Contains(got, "header_key") {
		t.Fatalf("expected header_key error, got: %s", got)
	}
}

func TestValidateBlueprintGraph_HeaderMap_EmptyMap(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ports.ResolverDef{
		Type:      "header_map",
		HeaderKey: "x-tenant",
		HeaderMap: map[string]string{},
	}
	if got := errorString(t, cfg); got == "" {
		t.Fatal("expected error for empty header_map")
	}
}

func TestValidateBlueprintGraph_HeaderMap_UnknownBinding(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ports.ResolverDef{
		Type:      "header_map",
		HeaderKey: "x-tenant",
		HeaderMap: map[string]string{"acme": "nonexistent"},
	}
	got := errorString(t, cfg)
	if got == "" {
		t.Fatal("expected error for unknown binding in header_map")
	}
	if !strings.Contains(got, "nonexistent") {
		t.Fatalf("expected binding ID in error, got: %s", got)
	}
}

func TestValidateBlueprintGraph_Rules_InvalidOperator(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ports.ResolverDef{
		Type: "rules",
		Rules: []ports.RuleDef{
			{BindingID: "b1", Match: []ports.ConditionDef{
				{Field: "subject", Operator: "bogus", Value: "x"},
			}},
		},
	}
	got := errorString(t, cfg)
	if got == "" {
		t.Fatal("expected error for invalid operator")
	}
	if !strings.Contains(got, "bogus") {
		t.Fatalf("expected operator in error, got: %s", got)
	}
}

func TestValidateBlueprintGraph_Rules_InvalidRegex(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ports.ResolverDef{
		Type: "rules",
		Rules: []ports.RuleDef{
			{BindingID: "b1", Match: []ports.ConditionDef{
				{Field: "subject", Operator: "regex", Value: "[invalid("},
			}},
		},
	}
	got := errorString(t, cfg)
	if got == "" {
		t.Fatal("expected error for invalid regex")
	}
	if !strings.Contains(got, "regex") {
		t.Fatalf("expected regex error, got: %s", got)
	}
}

func TestValidateBlueprintGraph_Rules_RegexTooLong(t *testing.T) {
	cfg := validRouteWithResolver()
	longPattern := strings.Repeat("a", validate.MaxRegexPatternLen+1)
	cfg.Routes[0].Resolver = &ports.ResolverDef{
		Type: "rules",
		Rules: []ports.RuleDef{
			{BindingID: "b1", Match: []ports.ConditionDef{
				{Field: "subject", Operator: "regex", Value: longPattern},
			}},
		},
	}
	got := errorString(t, cfg)
	if got == "" {
		t.Fatal("expected error for regex too long")
	}
	if !strings.Contains(got, "maximum length") {
		t.Fatalf("expected max length error, got: %s", got)
	}
}

func TestValidateBlueprintGraph_Rules_EmptyField(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ports.ResolverDef{
		Type: "rules",
		Rules: []ports.RuleDef{
			{BindingID: "b1", Match: []ports.ConditionDef{
				{Field: "", Operator: "eq", Value: "x"},
			}},
		},
	}
	got := errorString(t, cfg)
	if got == "" {
		t.Fatal("expected error for empty field")
	}
	if !strings.Contains(got, "field is required") {
		t.Fatalf("expected field error, got: %s", got)
	}
}

func TestValidateBlueprintGraph_Rules_EmptyRulesNoDefault(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ports.ResolverDef{
		Type:  "rules",
		Rules: []ports.RuleDef{},
	}
	if got := errorString(t, cfg); got == "" {
		t.Fatal("expected error for empty rules with no default")
	}
}

func TestValidateBlueprintGraph_AllType_Valid(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ports.ResolverDef{Type: "all"}
	if got := errorString(t, cfg); got != "" {
		t.Fatalf("expected no errors, got: %s", got)
	}
}

func TestValidateBlueprintGraph_StaticType_Valid(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ports.ResolverDef{Type: "static"}
	if got := errorString(t, cfg); got != "" {
		t.Fatalf("expected no errors, got: %s", got)
	}
}

// TestValidateBlueprintGraph_DuplicateRouteID exercises the
// cross-reference half of the moved logic to ensure duplicate IDs
// across the route section are caught here (not in the field-level
// checks left in config).
func TestValidateBlueprintGraph_DuplicateRouteID(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes = append(cfg.Routes, ports.RouteDef{
		ID:         "r1",
		ReceiverID: "rx1",
		Bindings:   []string{"b1"},
	})
	got := errorString(t, cfg)
	if got == "" {
		t.Fatal("expected error for duplicate route id")
	}
	if !strings.Contains(got, "duplicate id \"r1\"") {
		t.Fatalf("expected duplicate-id error, got: %s", got)
	}
}

// TestValidateBlueprintGraph_UnknownRouteBinding asserts that route
// bindings referencing missing bindings produce a graph error.
func TestValidateBlueprintGraph_UnknownRouteBinding(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Bindings = []string{"b1", "ghost"}
	got := errorString(t, cfg)
	if got == "" {
		t.Fatal("expected error for unknown route binding")
	}
	if !strings.Contains(got, "ghost") {
		t.Fatalf("expected binding-id error, got: %s", got)
	}
}

// TestValidateBlueprintGraph_DirectHoldEmitsWarning confirms the
// direct_hold fencing advisory is preserved in the moved code path.
func TestValidateBlueprintGraph_DirectHoldEmitsWarning(t *testing.T) {
	cfg := validRouteWithResolver()
	res := validate.ValidateBlueprintGraph(cfg)
	if res == nil {
		t.Fatal("expected non-nil result carrying the direct_hold warning")
	}
	if len(res.Warnings) == 0 {
		t.Fatalf("expected at least one warning, got none")
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "direct_hold") {
		t.Fatalf("expected direct_hold warning, got: %v", res.Warnings)
	}
}

// TestValidateBlueprintGraph_NilOnEmpty ensures the function returns
// nil when there are no routes/sections — no errors and no warnings.
func TestValidateBlueprintGraph_NilOnEmpty(t *testing.T) {
	cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{ID: "b"}}
	if res := validate.ValidateBlueprintGraph(cfg); res != nil {
		t.Fatalf("expected nil result for empty config, got: %+v", res)
	}
}

// TestValidateBlueprintGraph_ReceiverSessionTransport covers the
// ADV-F1-P2 guard: a receiver whose explicit transport differs from
// its session's transport fails validation, while a matching pair — or
// a receiver that inherits the session transport — passes.
func TestValidateBlueprintGraph_ReceiverSessionTransport(t *testing.T) {
	tests := []struct {
		name        string
		rxTransport string
		wantErr     bool
	}{
		{name: "mismatch fails", rxTransport: "amqp", wantErr: true},
		{name: "match passes", rxTransport: "mqtt", wantErr: false},
		{name: "inherited transport passes", rxTransport: "", wantErr: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &ports.BridgeConfig{
				Bridge:   ports.BridgeSettings{ID: "test"},
				Sessions: []ports.SessionDef{{ID: "s1", Transport: "mqtt"}},
				Receivers: []ports.ReceiverDef{
					{ID: "rx1", Transport: tc.rxTransport, SessionID: "s1"},
				},
			}
			got := errorString(t, cfg)
			if tc.wantErr {
				if !strings.Contains(got, "must share one transport") {
					t.Fatalf("expected transport-mismatch error, got: %q", got)
				}
			} else if got != "" {
				t.Fatalf("expected no errors, got: %q", got)
			}
		})
	}
}
