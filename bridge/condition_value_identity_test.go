package bridge

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// ruleWithValue is a minimal config whose only interesting content is one
// resolver rule's condition value.
func ruleWithValue(v any) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "demo"},
		Routes: []ports.RouteDef{{
			ID: "r",
			Resolver: &ports.ResolverDef{Type: "rules", Rules: []ports.RuleDef{{
				BindingID: "b",
				Match:     []ports.ConditionDef{{Field: "kind", Operator: "eq", Value: v}},
			}}},
		}},
	}
}

// TestConfigContentIdentity_ConditionValuesThatMatchDifferentlyStayDistinct
// pins the projection against the one place the empty-collection rule must not
// reach: a rule whose value is an empty map matches an empty object at runtime,
// a rule with no value matches null, and a rule whose value is an empty list
// matches nothing. Each is a different rule, so a reload between them is a
// change, not a no-op.
func TestConfigContentIdentity_ConditionValuesThatMatchDifferentlyStayDistinct(t *testing.T) {
	absent := ruleWithValue(nil)
	emptyMap := ruleWithValue(map[string]any{})
	emptyList := ruleWithValue([]any{})

	require.False(t, configContentEqual(absent, emptyMap))
	require.False(t, configContentEqual(absent, emptyList))
	require.False(t, configContentEqual(emptyMap, emptyList))

	seen := map[string]struct{}{}
	for _, cfg := range []*ports.BridgeConfig{absent, emptyMap, emptyList} {
		digest, ok := configCanonicalBytesDigest(cfg)
		require.True(t, ok)
		seen[digest] = struct{}{}
	}
	require.Len(t, seen, 3, "three rules that behave differently must have three identities")

	// The same rule written twice is still one rule.
	require.True(t, configContentEqual(ruleWithValue(map[string]any{"a": 1}), ruleWithValue(map[string]any{"a": 1})))

	// A literal string "[]" is a different rule from an empty list, and two
	// integers the runtime cannot tell apart (it compares float64) are one rule.
	require.False(t, configContentEqual(ruleWithValue("[]"), emptyList))
	require.True(t, configContentEqual(ruleWithValue(int64(9007199254740992)), ruleWithValue(int64(9007199254740993))))
}
