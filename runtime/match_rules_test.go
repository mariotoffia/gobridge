package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/runtime"
)

// ---------------------------------------------------------------------------
// MatchBySubjectPrefix
// ---------------------------------------------------------------------------

func TestMatchBySubjectPrefix_SelectsCorrectBinding(t *testing.T) {
	bindings := []routing.DestinationBinding{
		{ID: "orders-out", Address: "orders-queue"},
		{ID: "alerts-out", Address: "alerts-queue"},
	}
	prefixMap := map[string]string{
		"orders.": "orders-out",
		"alerts.": "alerts-out",
	}

	resolver := runtime.NewBindingResolver(bindings, runtime.MatchBySubjectPrefix(prefixMap))

	t.Run("orders_prefix", func(t *testing.T) {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "orders.created"})
		plans, err := resolver.Resolve(context.Background(), env)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if len(plans) != 1 || plans[0].BindingID != "orders-out" {
			t.Fatalf("expected orders-out, got %v", plans)
		}
	})

	t.Run("alerts_prefix", func(t *testing.T) {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "alerts.critical"})
		plans, err := resolver.Resolve(context.Background(), env)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if len(plans) != 1 || plans[0].BindingID != "alerts-out" {
			t.Fatalf("expected alerts-out, got %v", plans)
		}
	})

	t.Run("no_prefix_match", func(t *testing.T) {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "users.signup"})
		_, err := resolver.Resolve(context.Background(), env)
		if err == nil {
			t.Fatal("expected error for no prefix match")
		}
	})
}

// ---------------------------------------------------------------------------
// RuleResolver
// ---------------------------------------------------------------------------

func compileRules(t *testing.T, rules []runtime.MatchRule) []runtime.MatchRule {
	t.Helper()
	compiled, err := runtime.CompileMatchRules(rules)
	if err != nil {
		t.Fatalf("CompileMatchRules: %v", err)
	}
	return compiled
}

func makeRuleResolver(t *testing.T, bindings []routing.DestinationBinding, rules []runtime.MatchRule, defaultBinding string) *runtime.RuleResolver {
	t.Helper()
	compiled := compileRules(t, rules)
	resolver, err := runtime.NewRuleResolver(bindings, compiled, defaultBinding)
	if err != nil {
		t.Fatalf("NewRuleResolver: %v", err)
	}
	return resolver
}

func TestRuleResolver_FirstMatchWins(t *testing.T) {
	bindings := []routing.DestinationBinding{
		{ID: "first"}, {ID: "second"},
	}
	rules := []runtime.MatchRule{
		{BindingID: "first", Conditions: []runtime.MatchCondition{
			{Field: "subject", Operator: "prefix", Value: runtime.Val("orders.")},
		}},
		{BindingID: "second", Conditions: []runtime.MatchCondition{
			{Field: "subject", Operator: "prefix", Value: runtime.Val("orders.")},
		}},
	}
	resolver := makeRuleResolver(t, bindings, rules, "")

	env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "orders.new"})
	plans, err := resolver.Resolve(context.Background(), env)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected exactly 1 plan, got %d", len(plans))
	}
	if plans[0].BindingID != "first" {
		t.Fatalf("expected first-match-wins: got %q", plans[0].BindingID)
	}
}

func TestRuleResolver_SecondRuleMatches(t *testing.T) {
	bindings := []routing.DestinationBinding{
		{ID: "first"}, {ID: "second"},
	}
	rules := []runtime.MatchRule{
		{BindingID: "first", Conditions: []runtime.MatchCondition{
			{Field: "subject", Operator: "prefix", Value: runtime.Val("alerts.")},
		}},
		{BindingID: "second", Conditions: []runtime.MatchCondition{
			{Field: "subject", Operator: "prefix", Value: runtime.Val("orders.")},
		}},
	}
	resolver := makeRuleResolver(t, bindings, rules, "")

	env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "orders.new"})
	plans, err := resolver.Resolve(context.Background(), env)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plans[0].BindingID != "second" {
		t.Fatalf("expected second, got %q", plans[0].BindingID)
	}
}

func TestRuleResolver_DefaultBinding(t *testing.T) {
	bindings := []routing.DestinationBinding{
		{ID: "specific"}, {ID: "fallback"},
	}
	rules := []runtime.MatchRule{
		{BindingID: "specific", Conditions: []runtime.MatchCondition{
			{Field: "subject", Operator: "eq", Value: runtime.Val("exact")},
		}},
	}
	resolver := makeRuleResolver(t, bindings, rules, "fallback")

	env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "unmatched"})
	plans, err := resolver.Resolve(context.Background(), env)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plans[0].BindingID != "fallback" {
		t.Fatalf("expected fallback, got %q", plans[0].BindingID)
	}
}

func TestRuleResolver_NoMatch_NoDefault_Error(t *testing.T) {
	bindings := []routing.DestinationBinding{{ID: "only"}}
	rules := []runtime.MatchRule{
		{BindingID: "only", Conditions: []runtime.MatchCondition{
			{Field: "subject", Operator: "eq", Value: runtime.Val("exact")},
		}},
	}
	resolver := makeRuleResolver(t, bindings, rules, "")

	env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "unmatched"})
	_, err := resolver.Resolve(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for no match and no default")
	}
}

func TestRuleResolver_MultiConditionRule_AND(t *testing.T) {
	bindings := []routing.DestinationBinding{{ID: "target"}, {ID: "fallback"}}
	rules := []runtime.MatchRule{
		{BindingID: "target", Conditions: []runtime.MatchCondition{
			{Field: "subject", Operator: "prefix", Value: runtime.Val("orders.")},
			{Field: "header.priority", Operator: "eq", Value: runtime.Val("high")},
		}},
	}
	resolver := makeRuleResolver(t, bindings, rules, "fallback")

	t.Run("both_match", func(t *testing.T) {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			Subject: "orders.new",
			Headers: map[string]any{"priority": "high"},
		})
		plans, err := resolver.Resolve(context.Background(), env)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if plans[0].BindingID != "target" {
			t.Fatalf("expected target, got %s", plans[0].BindingID)
		}
	})

	t.Run("partial_match_falls_through", func(t *testing.T) {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			Subject: "orders.new",
			Headers: map[string]any{"priority": "low"},
		})
		plans, err := resolver.Resolve(context.Background(), env)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if plans[0].BindingID != "fallback" {
			t.Fatalf("expected fallback, got %s", plans[0].BindingID)
		}
	})
}

func TestRuleResolver_EmptyRules_DefaultOnly(t *testing.T) {
	bindings := []routing.DestinationBinding{{ID: "default-bind"}}
	resolver := makeRuleResolver(t, bindings, nil, "default-bind")

	env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "anything"})
	plans, err := resolver.Resolve(context.Background(), env)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plans[0].BindingID != "default-bind" {
		t.Fatalf("expected default-bind, got %s", plans[0].BindingID)
	}
}

func TestRuleResolver_NoConditions_AlwaysMatch(t *testing.T) {
	bindings := []routing.DestinationBinding{{ID: "catch-all"}}
	rules := []runtime.MatchRule{
		{BindingID: "catch-all", Conditions: nil},
	}
	resolver := makeRuleResolver(t, bindings, rules, "")

	env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "anything"})
	plans, err := resolver.Resolve(context.Background(), env)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plans[0].BindingID != "catch-all" {
		t.Fatalf("expected catch-all, got %s", plans[0].BindingID)
	}
}

func TestRuleResolver_JSONPayloadCondition(t *testing.T) {
	bindings := []routing.DestinationBinding{{ID: "high-prio"}, {ID: "low-prio"}}
	rules := []runtime.MatchRule{
		{BindingID: "high-prio", Conditions: []runtime.MatchCondition{
			{Field: "$.priority", Operator: "gt", Value: runtime.Val(float64(5))},
		}},
	}
	resolver := makeRuleResolver(t, bindings, rules, "low-prio")

	t.Run("high_priority", func(t *testing.T) {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte(`{"priority":10}`)})
		plans, _ := resolver.Resolve(context.Background(), env)
		if plans[0].BindingID != "high-prio" {
			t.Fatalf("expected high-prio, got %s", plans[0].BindingID)
		}
	})

	t.Run("low_priority_default", func(t *testing.T) {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte(`{"priority":2}`)})
		plans, _ := resolver.Resolve(context.Background(), env)
		if plans[0].BindingID != "low-prio" {
			t.Fatalf("expected low-prio, got %s", plans[0].BindingID)
		}
	})
}

func TestRuleResolver_RegexCondition(t *testing.T) {
	bindings := []routing.DestinationBinding{{ID: "order-match"}, {ID: "other"}}
	rules := []runtime.MatchRule{
		{BindingID: "order-match", Conditions: []runtime.MatchCondition{
			{Field: "subject", Operator: "regex", Value: runtime.Val(`^order-\d+$`)},
		}},
	}
	resolver := makeRuleResolver(t, bindings, rules, "other")

	env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "order-42"})
	plans, _ := resolver.Resolve(context.Background(), env)
	if plans[0].BindingID != "order-match" {
		t.Fatalf("expected order-match, got %s", plans[0].BindingID)
	}
}

func TestRuleResolver_WithAddressTemplate(t *testing.T) {
	bindings := []routing.DestinationBinding{
		{ID: "dynamic", Address: "events/{region}/stream"},
	}
	rules := []runtime.MatchRule{
		{BindingID: "dynamic", Conditions: []runtime.MatchCondition{
			{Field: "header.region", Operator: "exists", Value: runtime.Val(true)},
		}},
	}
	resolver := makeRuleResolver(t, bindings, rules, "")

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		Subject: "test",
		Headers: map[string]any{"region": "eu-west"},
	})
	plans, err := resolver.Resolve(context.Background(), env)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plans[0].Address != "events/eu-west/stream" {
		t.Fatalf("expected rendered address, got %q", plans[0].Address)
	}
}

func TestRuleResolver_UnknownBindingInRule_ReturnsError(t *testing.T) {
	compiled := compileRules(t, []runtime.MatchRule{
		{BindingID: "nonexistent", Conditions: nil},
	})
	_, err := runtime.NewRuleResolver(
		[]routing.DestinationBinding{{ID: "real"}},
		compiled,
		"",
	)
	if err == nil {
		t.Fatal("expected error for unknown binding in rule")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Fatalf("error should mention binding ID, got: %v", err)
	}
}

func TestRuleResolver_UnknownDefaultBinding_ReturnsError(t *testing.T) {
	_, err := runtime.NewRuleResolver(
		[]routing.DestinationBinding{{ID: "real"}},
		nil,
		"nonexistent",
	)
	if err == nil {
		t.Fatal("expected error for unknown default binding")
	}
}

func TestCompileMatchRules_InvalidRegex_ReturnsError(t *testing.T) {
	_, err := runtime.CompileMatchRules([]runtime.MatchRule{
		{BindingID: "bad", Conditions: []runtime.MatchCondition{
			{Field: "subject", Operator: "regex", Value: runtime.Val("[invalid(")},
		}},
	})
	if err == nil {
		t.Fatal("expected compile error for invalid regex")
	}
	if !strings.Contains(err.Error(), "rule 0") {
		t.Fatalf("error should identify rule index, got: %v", err)
	}
}

func TestRuleResolver_ConditionEvalError_TreatedAsNoMatch(t *testing.T) {
	bindings := []routing.DestinationBinding{{ID: "target"}, {ID: "fallback"}}
	rules := []runtime.MatchRule{
		{BindingID: "target", Conditions: []runtime.MatchCondition{
			// Numeric compare on non-numeric string -> error -> treated as no-match
			{Field: "header.x", Operator: "gt", Value: runtime.Val(float64(5))},
		}},
	}
	resolver := makeRuleResolver(t, bindings, rules, "fallback")

	env := messaging.MustEnvelope(messaging.EnvelopeInput{Headers: map[string]any{"x": "not-a-number"}})
	plans, err := resolver.Resolve(context.Background(), env)
	if err != nil {
		t.Fatalf("expected no error (eval error treated as no-match), got: %v", err)
	}
	if plans[0].BindingID != "fallback" {
		t.Fatalf("expected fallback, got %s", plans[0].BindingID)
	}
}
