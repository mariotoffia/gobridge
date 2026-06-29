package runtime

import (
	"context"
	"fmt"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
)

// ---------------------------------------------------------------------------
// Condition evaluator benchmarks
// ---------------------------------------------------------------------------

func BenchmarkConditionEval_Eq_Header(b *testing.B) {
	eval, _ := newConditionEval(MatchCondition{Field: "header.tenant", Operator: OpEquals, Value: Val("acme")})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Headers: map[string]any{"tenant": "acme"}})
	ctx := newEvalContext()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = eval.evaluate(env, ctx)
	}
}

func BenchmarkConditionEval_Prefix_Subject(b *testing.B) {
	eval, _ := newConditionEval(MatchCondition{Field: "subject", Operator: OpPrefix, Value: Val("orders.")})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "orders.created.eu-west"})
	ctx := newEvalContext()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = eval.evaluate(env, ctx)
	}
}

func BenchmarkConditionEval_Regex_Precompiled(b *testing.B) {
	eval, _ := newConditionEval(MatchCondition{Field: "subject", Operator: OpRegex, Value: Val(`^order-\d{4,8}$`)})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "order-12345678"})
	ctx := newEvalContext()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = eval.evaluate(env, ctx)
	}
}

func BenchmarkConditionEval_JSONPath_Shallow(b *testing.B) {
	eval, _ := newConditionEval(MatchCondition{Field: "$.status", Operator: OpEquals, Value: Val("active")})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte(`{"status":"active","count":42}`)})

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ctx := newEvalContext() // new context each iteration (parse once per envelope)
		_, _ = eval.evaluate(env, ctx)
	}
}

func BenchmarkConditionEval_JSONPath_Deep(b *testing.B) {
	eval, _ := newConditionEval(MatchCondition{Field: "$.order.item.status", Operator: OpEquals, Value: Val("shipped")})
	env := messaging.MustEnvelope(messaging.EnvelopeInput{Payload: []byte(`{"order":{"item":{"status":"shipped","qty":3}}}`)})

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ctx := newEvalContext()
		_, _ = eval.evaluate(env, ctx)
	}
}

// ---------------------------------------------------------------------------
// RuleResolver benchmarks
// ---------------------------------------------------------------------------

func BenchmarkRuleResolver_1Rule(b *testing.B) {
	benchRuleResolver(b, 1, 0)
}

func BenchmarkRuleResolver_10Rules_FirstMatch(b *testing.B) {
	benchRuleResolver(b, 10, 0)
}

func BenchmarkRuleResolver_10Rules_LastMatch(b *testing.B) {
	benchRuleResolver(b, 10, 9)
}

func BenchmarkRuleResolver_10Rules_NoMatch_Default(b *testing.B) {
	benchRuleResolver(b, 10, -1)
}

func BenchmarkRuleResolver_100Rules_MidMatch(b *testing.B) {
	benchRuleResolver(b, 100, 50)
}

func benchRuleResolver(b *testing.B, numRules int, matchIdx int) {
	b.Helper()

	bindings := make([]routing.DestinationBinding, numRules+1)
	rules := make([]MatchRule, numRules)

	for i := 0; i < numRules; i++ {
		bid := fmt.Sprintf("bind-%d", i)
		bindings[i] = routing.DestinationBinding{ID: bid, Address: fmt.Sprintf("topic-%d", i)}
		rules[i] = MatchRule{
			BindingID: bid,
			Conditions: []MatchCondition{
				{Field: "header.route-key", Operator: OpEquals, Value: Val(fmt.Sprintf("key-%d", i))},
			},
		}
	}
	bindings[numRules] = routing.DestinationBinding{ID: "default", Address: "default-topic"}

	compiled, _ := CompileMatchRules(rules)
	resolver, _ := NewRuleResolver(bindings, compiled, "default")

	var matchVal string
	if matchIdx >= 0 {
		matchVal = fmt.Sprintf("key-%d", matchIdx)
	} else {
		matchVal = "no-match"
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		Subject: "test",
		Headers: map[string]any{"route-key": matchVal},
	})

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = resolver.Resolve(context.Background(), env)
	}
}

// ---------------------------------------------------------------------------
// JSON payload caching benchmark
// ---------------------------------------------------------------------------

func BenchmarkEvalContext_CachedVsUncached(b *testing.B) {
	eval1, _ := newConditionEval(MatchCondition{Field: "$.order.id", Operator: OpEquals, Value: Val("42")})
	eval2, _ := newConditionEval(MatchCondition{Field: "$.order.status", Operator: OpEquals, Value: Val("active")})
	eval3, _ := newConditionEval(MatchCondition{Field: "$.order.priority", Operator: OpGreaterThan, Value: Val(float64(5))})

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		Payload: []byte(`{"order":{"id":"42","status":"active","priority":8}}`),
	})

	b.Run("cached_3_evals", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			ctx := newEvalContext()
			_, _ = eval1.evaluate(env, ctx)
			_, _ = eval2.evaluate(env, ctx)
			_, _ = eval3.evaluate(env, ctx)
		}
	})

	b.Run("uncached_3_evals", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			ctx1 := newEvalContext()
			_, _ = eval1.evaluate(env, ctx1)
			ctx2 := newEvalContext()
			_, _ = eval2.evaluate(env, ctx2)
			ctx3 := newEvalContext()
			_, _ = eval3.evaluate(env, ctx3)
		}
	})
}
