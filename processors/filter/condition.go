package filter

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
)

type conditionEvaluator struct {
	condition Condition
	regex     *regexp.Regexp
	// condNum is the pre-parsed numeric Condition.Value for ordering
	// operators (gt/lt/gte/lte), validated at construction so a
	// non-numeric comparison value fails the build, not every message.
	condNum float64
	// maxPayloadBytes is the effective payload-size ceiling for "$."
	// path evaluation, copied from the owning Processor's Config in
	// New. Zero means "no ceiling" (a bare evaluator constructed in a
	// test), so direct evaluator use is not size-limited.
	maxPayloadBytes int
}

func newConditionEvaluator(c Condition) (*conditionEvaluator, error) {
	if err := validateOperator(c.Operator); err != nil {
		return nil, err
	}

	eval := &conditionEvaluator{condition: c}

	switch c.Operator {
	case OperatorRegex:
		pattern, ok := c.Value.(string)
		if !ok {
			return nil, fmt.Errorf("regex operator requires string value")
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regex pattern: %w", err)
		}
		eval.regex = re
	case OperatorExists:
		// An absent / non-bool value must not silently default to
		// "false" ("not exists"): a "drop if x exists" rule missing its
		// bool would drop everything that LACKS x. Require an explicit
		// bool at construction.
		if _, ok := c.Value.(bool); !ok {
			return nil, ErrExistsValueNotBool.With("value", fmt.Sprintf("%v (%T)", c.Value, c.Value))
		}
	case OperatorIn:
		// A non-slice value makes "in" a constant false — a policy gate
		// that never matches. Fail the build instead.
		if c.Value == nil || reflect.ValueOf(c.Value).Kind() != reflect.Slice {
			return nil, ErrInValueNotSlice.With("value", fmt.Sprintf("%v (%T)", c.Value, c.Value))
		}
	case OperatorGreaterThan, OperatorLessThan, OperatorGreaterOrEqual, OperatorLessOrEqual:
		// The comparison value comes from configuration; a non-numeric
		// one is a deterministic misconfiguration. Validate here so the
		// per-message path can only fail on the (payload-derived) field
		// side.
		f, err := toFloat64(c.Value)
		if err != nil {
			return nil, ErrComparisonValueNotNumeric.
				With("operator", string(c.Operator)).
				Wrap(err)
		}
		eval.condNum = f
	}

	return eval, nil
}

// payloadDoc lazily parses an envelope's JSON payload ONCE per Process
// call and shares the parsed document read-only across every "$." path
// condition. Without it, each condition paid a defensive payload copy
// plus a full JSON parse (N conditions × payload size per message).
// Not safe for concurrent use; a Process call evaluates conditions
// sequentially.
type payloadDoc struct {
	parsed bool
	data   map[string]any
	err    error
}

// get returns the parsed payload object, parsing it on first use. The
// result (including a parse failure) is cached for subsequent
// conditions of the same Process call. Conditions must treat the
// returned map as read-only.
func (d *payloadDoc) get(env *messaging.Envelope, maxPayloadBytes int, path string) (map[string]any, error) {
	if d.parsed {
		return d.data, d.err
	}
	d.parsed = true

	payload := env.Payload()
	if len(payload) == 0 {
		return nil, nil
	}

	if maxPayloadBytes > 0 && len(payload) > maxPayloadBytes {
		d.err = shared.ErrPayloadTooLarge.Wrap(
			fmt.Errorf("filter: payload %d bytes exceeds limit %d", len(payload), maxPayloadBytes))
		return nil, d.err
	}

	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		// SECURITY: a JSON parse failure must NOT be a silent no-match.
		// Treating malformed JSON as "field absent" lets a hostile
		// producer bypass an ActionDrop (deny) rule by sending garbage.
		// Fail closed: classify the payload as rejected so the runtime
		// DLQs it instead of passing it down the chain.
		d.err = shared.ErrInvalidPayload.Wrap(
			fmt.Errorf("filter: %q path on unparseable JSON payload: %w", path, err))
		return nil, d.err
	}
	d.data = data
	return d.data, nil
}

// evaluate evaluates the condition against env with a private payload
// document (single-condition use; tests). The Processor shares one
// payloadDoc across all its conditions via evaluateDoc.
func (e *conditionEvaluator) evaluate(env *messaging.Envelope) (bool, error) {
	return e.evaluateDoc(env, &payloadDoc{})
}

func (e *conditionEvaluator) evaluateDoc(env *messaging.Envelope, doc *payloadDoc) (bool, error) {
	value, exists, err := e.extractField(env, doc)
	if err != nil {
		return false, err
	}

	if e.condition.Operator == OperatorExists {
		// Value is validated as an explicit bool at construction.
		expected, _ := e.condition.Value.(bool)
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

func (e *conditionEvaluator) extractField(env *messaging.Envelope, doc *payloadDoc) (any, bool, error) {
	field := e.condition.Field

	switch {
	case field == "subject":
		return env.Subject(), true, nil
	case strings.HasPrefix(field, "header."):
		key := strings.TrimPrefix(field, "header.")
		if env.Headers() == nil {
			return nil, false, nil
		}
		val, ok := env.Headers()[key]
		return val, ok, nil
	case strings.HasPrefix(field, "$."):
		return e.extractFromPayload(env, doc, field)
	default:
		if env.Headers() == nil {
			return nil, false, nil
		}
		val, ok := env.Headers()[field]
		return val, ok, nil
	}
}

func (e *conditionEvaluator) extractFromPayload(env *messaging.Envelope, doc *payloadDoc, path string) (any, bool, error) {
	data, err := doc.get(env, e.maxPayloadBytes, path)
	if err != nil {
		return nil, false, err
	}
	if data == nil {
		return nil, false, nil
	}

	path = strings.TrimPrefix(path, "$.")
	parts := strings.Split(path, ".")

	var current any = data
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

func (e *conditionEvaluator) compare(value any) (bool, error) {
	switch e.condition.Operator {
	case OperatorEquals:
		return e.equals(value), nil
	case OperatorNotEquals:
		return !e.equals(value), nil
	case OperatorContains:
		return e.contains(value), nil
	case OperatorRegex:
		return e.matchesRegex(value), nil
	case OperatorGreaterThan:
		cmp, err := e.numericCompare(value)
		return cmp > 0, err
	case OperatorLessThan:
		cmp, err := e.numericCompare(value)
		return cmp < 0, err
	case OperatorGreaterOrEqual:
		cmp, err := e.numericCompare(value)
		return cmp >= 0, err
	case OperatorLessOrEqual:
		cmp, err := e.numericCompare(value)
		return cmp <= 0, err
	case OperatorIn:
		return e.isIn(value), nil
	default:
		// Unreachable: validateOperator rejects unknown operators at
		// construction. Kept as a fail-closed, correctly classified
		// guard rather than a plain (runtime-retryable) fmt.Errorf.
		return false, shared.NewBridgeError(shared.ErrCodeInternal, shared.ErrorPermanent,
			fmt.Sprintf("filter: unhandled operator %q", e.condition.Operator))
	}
}

// validateOperator reports whether op is a supported comparison
// operator. It runs at construction so a misconfigured operator fails
// the build (structured setup error) instead of degrading to a
// per-message plain error the runtime would treat as retryable.
func validateOperator(op Operator) error {
	switch op {
	case OperatorEquals, OperatorNotEquals, OperatorContains, OperatorRegex,
		OperatorGreaterThan, OperatorLessThan, OperatorGreaterOrEqual,
		OperatorLessOrEqual, OperatorExists, OperatorIn:
		return nil
	default:
		return ErrUnknownOperator.With("operator", string(op))
	}
}

func (e *conditionEvaluator) equals(value any) bool {
	return looseEqual(value, e.condition.Value)
}

// looseEqual reports equality with cross-type numeric coercion. JSON
// numbers decode as float64 while configuration values are typically
// int; without coercion, {eq, 3} never matches a payload {"priority":3}
// and a deny filter fails open. Non-numeric values keep strict
// reflect.DeepEqual semantics (no string↔number coercion).
func looseEqual(a, b any) bool {
	if reflect.DeepEqual(a, b) {
		return true
	}
	an, ok := toExactNumber(a)
	if !ok {
		return false
	}
	bn, ok := toExactNumber(b)
	if !ok {
		return false
	}
	return an.equal(bn)
}

// exactNumber is an exact numeric representation used for equality: an
// int64, a uint64, or a float64. Keeping the integer forms exact avoids
// the float64 precision cliff above 2^53 (e.g. int64(2^53+1) must NOT
// equal float64(2^53)).
type exactNumber struct {
	kind numKind
	i    int64
	u    uint64
	f    float64
}

type numKind uint8

const (
	numInt numKind = iota
	numUint
	numFloat
)

// float64 bounds for an exact int64/uint64 round-trip. 1<<63 and 1<<64
// are exactly representable as float64, so `f < maxInt64AsFloat` is the
// correct exclusive upper bound.
const (
	maxInt64AsFloat  = float64(1 << 63)
	maxUint64AsFloat = float64(1 << 64)
)

func toExactNumber(v any) (exactNumber, bool) {
	switch x := v.(type) {
	case int:
		return exactNumber{kind: numInt, i: int64(x)}, true
	case int8:
		return exactNumber{kind: numInt, i: int64(x)}, true
	case int16:
		return exactNumber{kind: numInt, i: int64(x)}, true
	case int32:
		return exactNumber{kind: numInt, i: int64(x)}, true
	case int64:
		return exactNumber{kind: numInt, i: x}, true
	case uint:
		return exactNumber{kind: numUint, u: uint64(x)}, true
	case uint8:
		return exactNumber{kind: numUint, u: uint64(x)}, true
	case uint16:
		return exactNumber{kind: numUint, u: uint64(x)}, true
	case uint32:
		return exactNumber{kind: numUint, u: uint64(x)}, true
	case uint64:
		return exactNumber{kind: numUint, u: x}, true
	case float32:
		return exactNumber{kind: numFloat, f: float64(x)}, true
	case float64:
		return exactNumber{kind: numFloat, f: x}, true
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return exactNumber{kind: numInt, i: i}, true
		}
		if f, err := x.Float64(); err == nil {
			return exactNumber{kind: numFloat, f: f}, true
		}
		return exactNumber{}, false
	default:
		return exactNumber{}, false
	}
}

func (a exactNumber) equal(b exactNumber) bool {
	// Order the pair so mixed-kind cases are handled once.
	if a.kind > b.kind {
		a, b = b, a
	}
	switch {
	case a.kind == numInt && b.kind == numInt:
		return a.i == b.i
	case a.kind == numInt && b.kind == numUint:
		return a.i >= 0 && uint64(a.i) == b.u
	case a.kind == numInt && b.kind == numFloat:
		return floatEqualsInt64(b.f, a.i)
	case a.kind == numUint && b.kind == numUint:
		return a.u == b.u
	case a.kind == numUint && b.kind == numFloat:
		return floatEqualsUint64(b.f, a.u)
	default: // float / float
		return a.f == b.f
	}
}

// floatEqualsInt64 reports whether f exactly equals i. The float is
// converted toward the integer (exact when integral and in range), not
// the other way around, so values above 2^53 compare exactly.
func floatEqualsInt64(f float64, i int64) bool {
	if f != math.Trunc(f) || f < -maxInt64AsFloat || f >= maxInt64AsFloat {
		return false
	}
	return int64(f) == i
}

func floatEqualsUint64(f float64, u uint64) bool {
	if f != math.Trunc(f) || f < 0 || f >= maxUint64AsFloat {
		return false
	}
	return uint64(f) == u
}

func (e *conditionEvaluator) contains(value any) bool {
	strVal := fmt.Sprintf("%v", value)
	strCond := fmt.Sprintf("%v", e.condition.Value)
	return strings.Contains(strVal, strCond)
}

func (e *conditionEvaluator) matchesRegex(value any) bool {
	if e.regex == nil {
		return false
	}
	strVal := fmt.Sprintf("%v", value)
	return e.regex.MatchString(strVal)
}

func (e *conditionEvaluator) numericCompare(value any) (int, error) {
	v1, err := toFloat64(value)
	if err != nil {
		// The field value comes from the message; a non-numeric field is
		// deterministic for this payload, so a retry can never succeed.
		// Classify as rejected (invalid payload) so the runtime DLQs the
		// message instead of redelivering it forever.
		return 0, shared.ErrInvalidPayload.Wrap(fmt.Errorf(
			"filter: field %q is not numeric for %q comparison: %w",
			e.condition.Field, e.condition.Operator, err))
	}
	// The condition value was validated and parsed at construction.
	v2 := e.condNum

	if v1 < v2 {
		return -1, nil
	} else if v1 > v2 {
		return 1, nil
	}
	return 0, nil
}

func (e *conditionEvaluator) isIn(value any) bool {
	// Condition.Value is validated as a slice at construction.
	condVal := reflect.ValueOf(e.condition.Value)
	if condVal.Kind() != reflect.Slice {
		return false
	}
	for i := 0; i < condVal.Len(); i++ {
		if looseEqual(value, condVal.Index(i).Interface()) {
			return true
		}
	}
	return false
}

func toFloat64(v any) (float64, error) {
	switch val := v.(type) {
	case float64:
		return val, nil
	case float32:
		return float64(val), nil
	case int:
		return float64(val), nil
	case int64:
		return float64(val), nil
	case int32:
		return float64(val), nil
	case string:
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return 0, fmt.Errorf("parse float %q: %w", val, err)
		}
		return f, nil
	case json.Number:
		f, err := val.Float64()
		if err != nil {
			return 0, fmt.Errorf("json.Number to float: %w", err)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("cannot convert %T to float64", v)
	}
}
