package runtime

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// Operator is the typed enumeration of MatchCondition operators. The
// underlying type is string so YAML / JSON loaders can deserialise
// directly into Operator without an intermediate string. The full
// closed set of valid operators is enumerated by the Op* constants
// below.
type Operator string

// Operator constants for condition matching. The values are stable on-
// the-wire identifiers also used by the validation layer in
// config/validate_resolver.go.
const (
	OpEquals      Operator = "eq"
	OpNotEquals   Operator = "ne"
	OpPrefix      Operator = "prefix"
	OpContains    Operator = "contains"
	OpRegex       Operator = "regex"
	OpGreaterThan Operator = "gt"
	OpLessThan    Operator = "lt"
	OpGTE         Operator = "gte"
	OpLTE         Operator = "lte"
	OpExists      Operator = "exists"
	OpIn          Operator = "in"
)

const (
	maxRegexPatternLen = 4096
	maxRegexInputLen   = 64 * 1024 // 64 KB
)

// ValueKind tags the storage slot that holds a ConditionValue's payload.
// It exists so condition evaluation can switch on a single byte rather
// than reach for reflect.TypeOf.
type ValueKind uint8

const (
	// KindNil represents an unset / absent ConditionValue (used for
	// operators that take no rhs, e.g. OpExists with default
	// semantics).
	KindNil ValueKind = iota
	// KindString — string slot.
	KindString
	// KindFloat — float64 slot.
	KindFloat
	// KindBool — bool slot.
	KindBool
	// KindStringList — homogeneous []string slot.
	KindStringList
	// KindFloatList — homogeneous []float64 slot.
	KindFloatList
	// KindAnyList — heterogeneous []ConditionValue slot used as a
	// fallback when an OpIn rhs cannot be coerced to a homogeneous
	// list of strings or floats.
	KindAnyList
)

// ConditionValue is a small typed sum that replaces the previous
// `Value any` field on MatchCondition. The kind tag selects which
// concrete slot is populated, eliminating reflect.TypeOf / reflect.
// DeepEqual on the per-envelope evaluation path. Construction is the
// only place that performs coercion (via Val); evaluation is
// reflection-free except for a single OpIn heterogeneous-list
// fallback that legacy callers can opt into.
type ConditionValue struct {
	kind   ValueKind
	str    string
	flt    float64
	bln    bool
	sList  []string
	fList  []float64
	anList []ConditionValue
}

// Kind returns the populated slot tag.
func (v ConditionValue) Kind() ValueKind { return v.kind }

// String returns the string slot. Caller is responsible for checking
// Kind() == KindString first.
func (v ConditionValue) String() string { return v.str }

// Float returns the float slot. Caller is responsible for checking
// Kind() == KindFloat first.
func (v ConditionValue) Float() float64 { return v.flt }

// Bool returns the bool slot.
func (v ConditionValue) Bool() bool { return v.bln }

// StringVal constructs a ConditionValue holding a string.
func StringVal(s string) ConditionValue { return ConditionValue{kind: KindString, str: s} }

// FloatVal constructs a ConditionValue holding a float64.
func FloatVal(f float64) ConditionValue { return ConditionValue{kind: KindFloat, flt: f} }

// BoolVal constructs a ConditionValue holding a bool.
func BoolVal(b bool) ConditionValue { return ConditionValue{kind: KindBool, bln: b} }

// StringListVal constructs a ConditionValue holding a homogeneous
// []string. The slice is referenced directly; callers that mutate it
// after construction will affect evaluation.
func StringListVal(ss []string) ConditionValue {
	return ConditionValue{kind: KindStringList, sList: ss}
}

// FloatListVal constructs a ConditionValue holding a homogeneous
// []float64. The slice is referenced directly.
func FloatListVal(fs []float64) ConditionValue {
	return ConditionValue{kind: KindFloatList, fList: fs}
}

// Val coerces an arbitrary Go value into a ConditionValue. This is
// the single coercion site in the rule pipeline: configuration loaders
// and tests pass `any` once at compile-time of the rule set, after
// which per-envelope evaluation never reaches for reflect.
//
// Coercion rules:
//   - string                      -> StringVal
//   - bool                        -> BoolVal
//   - any numeric Go type         -> FloatVal (float64)
//   - json.Number                 -> FloatVal (parsed)
//   - []string                    -> StringListVal
//   - []float64 / []int           -> FloatListVal
//   - []any                       -> homogeneous list when every
//     element coerces to the same scalar slot, otherwise KindAnyList
//     holding the per-element coercion
//   - nil                         -> KindNil
//   - anything else               -> stringified via fmt.Sprint
func Val(v any) ConditionValue {
	switch val := v.(type) {
	case nil:
		return ConditionValue{kind: KindNil}
	case ConditionValue:
		return val
	case string:
		return StringVal(val)
	case bool:
		return BoolVal(val)
	case float64:
		return FloatVal(val)
	case float32:
		return FloatVal(float64(val))
	case int:
		return FloatVal(float64(val))
	case int8:
		return FloatVal(float64(val))
	case int16:
		return FloatVal(float64(val))
	case int32:
		return FloatVal(float64(val))
	case int64:
		return FloatVal(float64(val))
	case uint:
		return FloatVal(float64(val))
	case uint8:
		return FloatVal(float64(val))
	case uint16:
		return FloatVal(float64(val))
	case uint32:
		return FloatVal(float64(val))
	case uint64:
		return FloatVal(float64(val))
	case json.Number:
		if f, err := val.Float64(); err == nil {
			return FloatVal(f)
		}
		return StringVal(val.String())
	case []string:
		return StringListVal(val)
	case []float64:
		return FloatListVal(val)
	case []int:
		fs := make([]float64, len(val))
		for i, x := range val {
			fs[i] = float64(x)
		}
		return FloatListVal(fs)
	case []any:
		return coerceAnyList(val)
	default:
		return StringVal(fmt.Sprint(v))
	}
}

func coerceAnyList(items []any) ConditionValue {
	if len(items) == 0 {
		return ConditionValue{kind: KindAnyList}
	}
	allString := true
	allNumeric := true
	floats := make([]float64, 0, len(items))
	strs := make([]string, 0, len(items))
	for _, it := range items {
		cv := Val(it)
		switch cv.kind {
		case KindString:
			allNumeric = false
			strs = append(strs, cv.str)
		case KindFloat:
			allString = false
			floats = append(floats, cv.flt)
		default:
			allNumeric = false
			allString = false
		}
	}
	if allNumeric {
		return FloatListVal(floats)
	}
	if allString {
		return StringListVal(strs)
	}
	cvs := make([]ConditionValue, len(items))
	for i, it := range items {
		cvs[i] = Val(it)
	}
	return ConditionValue{kind: KindAnyList, anList: cvs}
}

// MatchCondition defines a single field-level predicate for resolver
// matching. The Operator and Value are typed (Operator enum,
// ConditionValue typed sum) so condition evaluation does not require
// per-call reflect.TypeOf or reflect.DeepEqual.
type MatchCondition struct {
	Field    string
	Operator Operator
	Value    ConditionValue
}

// conditionEval is a pre-compiled condition evaluator. Regex patterns
// are compiled once at construction and reused across all evaluations.
type conditionEval struct {
	cond  MatchCondition
	regex *regexp.Regexp
}

// newConditionEval creates a compiled evaluator. Returns an error if
// the operator is OpRegex and the pattern is invalid or exceeds the
// length limit.
func newConditionEval(c MatchCondition) (*conditionEval, error) {
	eval := &conditionEval{cond: c}

	if c.Operator == OpRegex {
		if c.Value.kind != KindString {
			return nil, fmt.Errorf("regex operator requires string value, got kind %d", c.Value.kind)
		}
		pattern := c.Value.str
		if len(pattern) > maxRegexPatternLen {
			return nil, fmt.Errorf("regex pattern exceeds maximum length of %d characters", maxRegexPatternLen)
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regex pattern: %w", err)
		}
		eval.regex = re
	}

	return eval, nil
}

// evaluate checks the condition against an envelope using a shared
// parsed payload context. The parsedPayload may be nil; it will be
// lazily populated on first JSON path access.
func (e *conditionEval) evaluate(env *messaging.Envelope, ctx *evalContext) (bool, error) {
	value, exists, err := e.extractField(env, ctx)
	if err != nil {
		return false, fmt.Errorf("runtime: condition: extract field: %w", err)
	}

	if e.cond.Operator == OpExists {
		// Default expectation is "must exist"; an explicit BoolVal(false)
		// inverts the check (caller wants "must not exist").
		expected := true
		if e.cond.Value.kind == KindBool {
			expected = e.cond.Value.bln
		}
		if !expected {
			return !exists, nil
		}
		return exists, nil
	}

	if !exists {
		return false, nil
	}

	return e.compare(value)
}

// extractField retrieves the value for the condition's field path.
// Header lookups go through the typed messaging.Headers accessors
// (Get / nil-safe by design) rather than direct map indexing — this
// is the / hot-path migration site.
func (e *conditionEval) extractField(env *messaging.Envelope, ctx *evalContext) (any, bool, error) {
	field := e.cond.Field

	switch {
	case field == "subject":
		return env.Subject(), true, nil

	case strings.HasPrefix(field, "header."):
		key := strings.TrimPrefix(field, "header.")
		val, ok := env.Headers().Get(key)
		return val, ok, nil

	case strings.HasPrefix(field, "$."):
		return ctx.extractPayloadPath(env.Payload(), field)

	default:
		val, ok := env.Headers().Get(field)
		return val, ok, nil
	}
}

func (e *conditionEval) compare(value any) (bool, error) {
	switch e.cond.Operator {
	case OpEquals:
		return e.equals(value), nil
	case OpNotEquals:
		return !e.equals(value), nil
	case OpPrefix:
		return strings.HasPrefix(stringify(value), e.cond.Value.str), nil
	case OpContains:
		return strings.Contains(stringify(value), e.cond.Value.str), nil
	case OpRegex:
		return e.regexMatch(value), nil
	case OpGreaterThan:
		cmp, err := e.numericCompare(value)
		return cmp > 0, err
	case OpLessThan:
		cmp, err := e.numericCompare(value)
		return cmp < 0, err
	case OpGTE:
		cmp, err := e.numericCompare(value)
		return cmp >= 0, err
	case OpLTE:
		cmp, err := e.numericCompare(value)
		return cmp <= 0, err
	case OpIn:
		return e.isIn(value), nil
	default:
		return false, fmt.Errorf("unsupported operator: %s", e.cond.Operator)
	}
}

// equals compares the extracted lhs (any) to the typed rhs slot
// without reflection. Numeric comparisons go through float64
// normalisation so json.Number and concrete int* / float* types
// unify.
func (e *conditionEval) equals(value any) bool {
	switch e.cond.Value.kind {
	case KindString:
		return stringify(value) == e.cond.Value.str
	case KindFloat:
		f, err := condToFloat64(value)
		if err != nil {
			return false
		}
		return f == e.cond.Value.flt
	case KindBool:
		b, ok := value.(bool)
		if !ok {
			return false
		}
		return b == e.cond.Value.bln
	case KindNil:
		return value == nil
	default:
		return false
	}
}

func (e *conditionEval) regexMatch(value any) bool {
	if e.regex == nil {
		return false
	}
	strVal := stringify(value)
	if len(strVal) > maxRegexInputLen {
		return false
	}
	return e.regex.MatchString(strVal)
}

func (e *conditionEval) numericCompare(value any) (int, error) {
	v1, err := condToFloat64(value)
	if err != nil {
		return 0, fmt.Errorf("runtime: condition: convert lhs: %w", err)
	}
	if e.cond.Value.kind != KindFloat {
		return 0, fmt.Errorf("runtime: condition: rhs is not numeric (kind=%d)", e.cond.Value.kind)
	}
	v2 := e.cond.Value.flt

	if v1 < v2 {
		return -1, nil
	} else if v1 > v2 {
		return 1, nil
	}
	return 0, nil
}

func (e *conditionEval) isIn(value any) bool {
	switch e.cond.Value.kind {
	case KindStringList:
		s := stringify(value)
		for _, x := range e.cond.Value.sList {
			if x == s {
				return true
			}
		}
		return false
	case KindFloatList:
		f, err := condToFloat64(value)
		if err != nil {
			return false
		}
		for _, x := range e.cond.Value.fList {
			if x == f {
				return true
			}
		}
		return false
	case KindAnyList:
		// Heterogeneous fallback: per-element typed compare,
		// followed by deep-equality on the raw any for legacy
		// shapes (e.g. nested maps in test scaffolding). This is
		// the only remaining reflect call on the evaluation path
		// and runs only when the rule was authored with mixed-type
		// values.
		for _, item := range e.cond.Value.anList {
			tmp := &conditionEval{cond: MatchCondition{Operator: OpEquals, Value: item}}
			if tmp.equals(value) {
				return true
			}
		}
		for _, item := range e.cond.Value.anList {
			if reflect.DeepEqual(value, conditionValueToAny(item)) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// conditionValueToAny converts a typed slot back to its underlying Go
// value. Used only by the OpIn heterogeneous fallback, never on the
// hot path.
func conditionValueToAny(v ConditionValue) any {
	switch v.kind {
	case KindString:
		return v.str
	case KindFloat:
		return v.flt
	case KindBool:
		return v.bln
	case KindStringList:
		return v.sList
	case KindFloatList:
		return v.fList
	default:
		return nil
	}
}

// stringify renders an arbitrary Go value as a string for prefix /
// contains / regex / in-by-string operators. Strings pass through
// unchanged; everything else goes through fmt.Sprint.
func stringify(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func condToFloat64(v any) (float64, error) {
	var f float64
	var err error

	switch val := v.(type) {
	case float64:
		f = val
	case float32:
		f = float64(val)
	case int:
		f = float64(val)
	case int64:
		f = float64(val)
	case int32:
		f = float64(val)
	case int16:
		f = float64(val)
	case int8:
		f = float64(val)
	case uint:
		f = float64(val)
	case uint64:
		f = float64(val)
	case uint32:
		f = float64(val)
	case uint16:
		f = float64(val)
	case uint8:
		f = float64(val)
	case string:
		f, err = strconv.ParseFloat(val, 64)
		if err != nil {
			return 0, fmt.Errorf("cannot convert %q to float64", val)
		}
	case json.Number:
		f, err = val.Float64()
		if err != nil {
			return 0, fmt.Errorf("cannot convert json.Number to float64: %w", err)
		}
	default:
		return 0, fmt.Errorf("cannot convert %T to float64", v)
	}

	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("non-finite numeric value: %v", f)
	}

	return f, nil
}

// evalContext provides a lazily-parsed JSON payload cache shared
// across all condition evaluations for a single envelope. This avoids
// parsing the payload N*M times (N rules x M conditions).
type evalContext struct {
	parsed    map[string]any
	parseDone bool
	parseErr  bool
}

func newEvalContext() *evalContext {
	return &evalContext{}
}

func (ec *evalContext) extractPayloadPath(payload []byte, path string) (any, bool, error) {
	if !ec.parseDone {
		ec.parseDone = true
		if len(payload) == 0 {
			ec.parseErr = true
		} else {
			var data map[string]any
			if err := json.Unmarshal(payload, &data); err != nil {
				ec.parseErr = true
			} else {
				ec.parsed = data
			}
		}
	}

	if ec.parseErr || ec.parsed == nil {
		return nil, false, nil
	}

	path = strings.TrimPrefix(path, "$.")
	parts := strings.Split(path, ".")

	var current any = ec.parsed
	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		val, ok := m[part]
		if !ok {
			return nil, false, nil
		}
		current = val
	}

	return current, true, nil
}
