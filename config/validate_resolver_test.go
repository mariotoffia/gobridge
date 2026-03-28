package config

import (
	"strings"
	"testing"
)

func validRouteWithResolver() *BridgeConfig {
	return &BridgeConfig{
		Bridge: BridgeSettings{ID: "test"},
		Receivers: []ReceiverDef{
			{ID: "rx1", Transport: "http"},
		},
		Senders: []SenderDef{
			{ID: "tx1", Transport: "http"},
			{ID: "tx2", Transport: "http"},
		},
		Bindings: []BindingDef{
			{ID: "b1", SenderID: "tx1", Address: "/events/a"},
			{ID: "b2", SenderID: "tx2", Address: "/events/b"},
		},
		Routes: []RouteDef{
			{
				ID:         "r1",
				ReceiverID: "rx1",
				Bindings:   []string{"b1", "b2"},
			},
		},
	}
}

func TestValidateResolver_RulesType_Valid(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ResolverDef{
		Type:           "rules",
		DefaultBinding: "b2",
		Rules: []RuleDef{
			{BindingID: "b1", Match: []ConditionDef{
				{Field: "subject", Operator: "prefix", Value: "orders."},
			}},
		},
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
}

func TestValidateResolver_InvalidType(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ResolverDef{Type: "unknown"}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for invalid resolver type")
	}
	if !strings.Contains(err.Error(), "resolver.type") {
		t.Fatalf("expected resolver.type error, got: %v", err)
	}
}

func TestValidateResolver_DefaultBindingNotInRoute(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ResolverDef{
		Type:           "rules",
		DefaultBinding: "nonexistent",
		Rules:          []RuleDef{{BindingID: "b1"}},
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for unknown default_binding")
	}
	if !strings.Contains(err.Error(), "default_binding") {
		t.Fatalf("expected default_binding error, got: %v", err)
	}
}

func TestValidateResolver_RuleBindingNotInRoute(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ResolverDef{
		Type: "rules",
		Rules: []RuleDef{
			{BindingID: "nonexistent", Match: []ConditionDef{
				{Field: "subject", Operator: "eq", Value: "x"},
			}},
		},
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for rule referencing unknown binding")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Fatalf("expected binding ID in error, got: %v", err)
	}
}

func TestValidateResolver_HeaderMap_Valid(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ResolverDef{
		Type:      "header_map",
		HeaderKey: "x-tenant",
		HeaderMap: map[string]string{"acme": "b1", "globex": "b2"},
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestValidateResolver_HeaderMap_MissingKey(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ResolverDef{
		Type:      "header_map",
		HeaderMap: map[string]string{"acme": "b1"},
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for missing header_key")
	}
	if !strings.Contains(err.Error(), "header_key") {
		t.Fatalf("expected header_key error, got: %v", err)
	}
}

func TestValidateResolver_HeaderMap_EmptyMap(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ResolverDef{
		Type:      "header_map",
		HeaderKey: "x-tenant",
		HeaderMap: map[string]string{},
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for empty header_map")
	}
}

func TestValidateResolver_HeaderMap_UnknownBinding(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ResolverDef{
		Type:      "header_map",
		HeaderKey: "x-tenant",
		HeaderMap: map[string]string{"acme": "nonexistent"},
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for unknown binding in header_map")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Fatalf("expected binding ID in error, got: %v", err)
	}
}

func TestValidateResolver_Rules_InvalidOperator(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ResolverDef{
		Type: "rules",
		Rules: []RuleDef{
			{BindingID: "b1", Match: []ConditionDef{
				{Field: "subject", Operator: "bogus", Value: "x"},
			}},
		},
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for invalid operator")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("expected operator in error, got: %v", err)
	}
}

func TestValidateResolver_Rules_InvalidRegex(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ResolverDef{
		Type: "rules",
		Rules: []RuleDef{
			{BindingID: "b1", Match: []ConditionDef{
				{Field: "subject", Operator: "regex", Value: "[invalid("},
			}},
		},
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
	if !strings.Contains(err.Error(), "regex") {
		t.Fatalf("expected regex error, got: %v", err)
	}
}

func TestValidateResolver_Rules_RegexTooLong(t *testing.T) {
	cfg := validRouteWithResolver()
	longPattern := strings.Repeat("a", maxRegexPatternLen+1)
	cfg.Routes[0].Resolver = &ResolverDef{
		Type: "rules",
		Rules: []RuleDef{
			{BindingID: "b1", Match: []ConditionDef{
				{Field: "subject", Operator: "regex", Value: longPattern},
			}},
		},
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for regex too long")
	}
	if !strings.Contains(err.Error(), "maximum length") {
		t.Fatalf("expected max length error, got: %v", err)
	}
}

func TestValidateResolver_Rules_EmptyField(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ResolverDef{
		Type: "rules",
		Rules: []RuleDef{
			{BindingID: "b1", Match: []ConditionDef{
				{Field: "", Operator: "eq", Value: "x"},
			}},
		},
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for empty field")
	}
	if !strings.Contains(err.Error(), "field is required") {
		t.Fatalf("expected field error, got: %v", err)
	}
}

func TestValidateResolver_Rules_EmptyRulesNoDefault(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ResolverDef{
		Type:  "rules",
		Rules: []RuleDef{},
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for empty rules with no default")
	}
}

func TestValidateResolver_AllType_Valid(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ResolverDef{Type: "all"}
	if err := Validate(cfg); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestValidateResolver_StaticType_Valid(t *testing.T) {
	cfg := validRouteWithResolver()
	cfg.Routes[0].Resolver = &ResolverDef{Type: "static"}
	if err := Validate(cfg); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}
