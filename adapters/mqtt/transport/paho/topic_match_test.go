package paho

import "testing"

// TestMatchTopicFilter pins the MQTT v5 §4.7 topic-filter matching used
// for per-receiver dispatch isolation: single-level (+) and multi-level
// (#) wildcards, $share prefix stripping, and the $-topic exclusion.
func TestMatchTopicFilter(t *testing.T) {
	cases := []struct {
		name   string
		filter string
		topic  string
		want   bool
	}{
		// Exact matches.
		{"exact", "a/b/c", "a/b/c", true},
		{"exact mismatch", "a/b/c", "a/b/d", false},
		{"exact shorter topic", "a/b/c", "a/b", false},
		{"exact longer topic", "a/b", "a/b/c", false},
		{"root", "a", "a", true},
		{"empty filter is invalid", "", "", false},

		// Single-level wildcard.
		{"plus middle", "a/+/c", "a/b/c", true},
		{"plus middle mismatch depth", "a/+/c", "a/b/c/d", false},
		{"plus leaf", "a/b/+", "a/b/c", true},
		{"plus leaf empty level", "a/b/+", "a/b/", true},
		{"plus root", "+", "a", true},
		{"plus root not multilevel", "+", "a/b", false},
		{"plus multiple", "+/+", "a/b", true},

		// Multi-level wildcard.
		{"hash all", "#", "a/b/c", true},
		{"hash suffix", "a/#", "a/b/c", true},
		{"hash matches parent", "a/#", "a", true},
		{"hash wrong branch", "a/#", "b/c", false},
		{"hash after plus", "+/#", "a/b/c", true},

		// $-topics must not be matched by wildcard-leading filters.
		{"dollar not matched by hash", "#", "$SYS/broker/load", false},
		{"dollar not matched by plus", "+/broker/load", "$SYS/broker/load", false},
		{"dollar exact still matches", "$SYS/broker/load", "$SYS/broker/load", true},
		{"dollar prefix with hash", "$SYS/#", "$SYS/broker/load", true},

		// Shared subscriptions: the $share/<group>/ prefix is transparent.
		{"shared exact", "$share/g1/a/b", "a/b", true},
		{"shared wildcard", "$share/g1/a/#", "a/b/c", true},
		{"shared mismatch", "$share/g1/a/b", "a/c", false},

		// Case sensitivity.
		{"case sensitive", "A/b", "a/b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchTopicFilter(tc.filter, tc.topic); got != tc.want {
				t.Fatalf("matchTopicFilter(%q, %q) = %v, want %v", tc.filter, tc.topic, got, tc.want)
			}
		})
	}
}

// TestMatchesAnyFilter pins the empty-filter-list semantics (match all —
// the legacy single-receiver behaviour) and the any-of contract.
func TestMatchesAnyFilter(t *testing.T) {
	if !matchesAnyFilter(nil, "a/b") {
		t.Fatal("nil filter list must match every topic (legacy receiver)")
	}
	if !matchesAnyFilter([]string{"x/y", "a/+"}, "a/b") {
		t.Fatal("second filter should match")
	}
	if matchesAnyFilter([]string{"x/y", "z/#"}, "a/b") {
		t.Fatal("no filter matches; want false")
	}
}
