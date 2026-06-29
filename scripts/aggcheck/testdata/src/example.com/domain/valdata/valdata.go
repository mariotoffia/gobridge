// Package valdata is aggcheck analysistest fixture data for the
// domain-scoped heuristic track. Its import path contains "/domain/" so
// the value-aggregate heuristic (unexported field + value-receiver
// method returning the type itself) runs here.
package valdata

// HeuristicAgg trips the legacy heuristic: it has an unexported field and
// a value-receiver method returning itself, lives outside an
// _aggregate.go file, and declares no Validate(), so both heuristic rules
// fire.
type HeuristicAgg struct { // want `aggregate-like type "HeuristicAgg" in valdata.go should live in a file ending '_aggregate.go'` `aggregate-like type "HeuristicAgg" \(in valdata.go\) must declare a Validate\(\) error method`
	state int
}

// With returns a new HeuristicAgg — the value transition the heuristic
// keys on.
func (h HeuristicAgg) With(s int) HeuristicAgg { return HeuristicAgg{state: s} }

// MarkedValueObject has the same heuristic shape (unexported field +
// value-receiver method returning itself) but carries the value-object
// marker, so the heuristic stays silent.
//
// value-object
type MarkedValueObject struct {
	state int
}

// With returns a re-moded copy of itself.
func (m MarkedValueObject) With(s int) MarkedValueObject { return MarkedValueObject{state: s} }
