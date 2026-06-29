package main

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// TestAggregateMarkerRules proves the marker-track rules: a compliant
// marked aggregate (CleanRecord) passes; a marked aggregate with
// exported mutable fields (LeakyFields) is flagged per field; a marked
// aggregate with an unguarded pointer transition (Unguarded.SetStatus)
// is flagged; and an unmarked type (NotAnAggregate) is left alone.
func TestAggregateMarkerRules(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), Analyzer, "aggdata")
}

// TestValueObjectHeuristic proves the domain-scoped heuristic track: a
// heuristic-shaped type (unexported field + value-receiver method
// returning itself) is flagged, while the same shape carrying the
// "// value-object" marker is exempt.
func TestValueObjectHeuristic(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), Analyzer, "example.com/domain/valdata")
}
