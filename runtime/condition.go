package runtime

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/mariotoffia/gobridge/domain"
)

// Operator constants for condition matching.
const (
	OpEquals      = "eq"
	OpNotEquals   = "ne"
	OpPrefix      = "prefix"
	OpContains    = "contains"
	OpRegex       = "regex"
	OpGreaterThan = "gt"
	OpLessThan    = "lt"
	OpGTE         = "gte"
	OpLTE         = "lte"
	OpExists      = "exists"
	OpIn          = "in"

	maxRegexPatternLen = 4096
	maxRegexInputLen   = 64 * 1024 // 64 KB
)

// MatchCondition defines a single field-level predicate for resolver matching.
type MatchCondition struct {
	Field    string
	Operator string
	Value    any
}

// conditionEval is a pre-compiled condition evaluator. Regex patterns are
// compiled once at construction and reused across all evaluations.
type conditionEval struct {
	cond  MatchCondition
	regex *regexp.Regexp
}

// newConditionEval creates a compiled evaluator. Returns an error if the
// operator is "regex" and the pattern is invalid or exceeds the length limit.
func newConditionEval(c MatchCondition) (*conditionEval, error) {
	eval := &conditionEval{cond: c}

	if c.Operator == OpRegex {
		pattern, ok := c.Value.(string)
		if !ok {
			return nil, fmt.Errorf("regex operator requires string value, got %T", c.Value)
		}
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

// evaluate checks the condition against an envelope using a shared parsed
// payload context. The parsedPayload may be nil; it will be lazily populated
// on first JSON path access.
func (e *conditionEval) evaluate(env *domain.Envelope, ctx *evalContext) (bool, error) {
	value, exists, err := e.extractField(env, ctx)
	if err != nil {
		return false, err
	}

	if e.cond.Operator == OpExists {
		expected, _ := e.cond.Value.(bool)
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
func (e *conditionEval) extractField(env *domain.Envelope, ctx *evalContext) (any, bool, error) {
	field := e.cond.Field

	switch {
	case field == "subject":
		return env.Subject, true, nil

	case strings.HasPrefix(field, "header."):
		key := strings.TrimPrefix(field, "header.")
		if env.Headers == nil {
			return nil, false, nil
		}
		val, ok := env.Headers[key]
		return val, ok, nil

	case strings.HasPrefix(field, "$."):
		return ctx.extractPayloadPath(env.Payload, field)

	default:
		if env.Headers == nil {
			return nil, false, nil
		}
		val, ok := env.Headers[field]
		return val, ok, nil
	}
}

func (e *conditionEval) compare(value any) (bool, error) {
	switch e.cond.Operator {
	case OpEquals:
		return reflect.DeepEqual(value, e.cond.Value), nil
	case OpNotEquals:
		return !reflect.DeepEqual(value, e.cond.Value), nil
	case OpPrefix:
		return e.prefixMatch(value), nil
	case OpContains:
		return e.containsMatch(value), nil
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

func (e *conditionEval) prefixMatch(value any) bool {
	strVal := fmt.Sprintf("%v", value)
	strCond := fmt.Sprintf("%v", e.cond.Value)
	return strings.HasPrefix(strVal, strCond)
}

func (e *conditionEval) containsMatch(value any) bool {
	strVal := fmt.Sprintf("%v", value)
	strCond := fmt.Sprintf("%v", e.cond.Value)
	return strings.Contains(strVal, strCond)
}

func (e *conditionEval) regexMatch(value any) bool {
	if e.regex == nil {
		return false
	}
	strVal := fmt.Sprintf("%v", value)
	if len(strVal) > maxRegexInputLen {
		return false
	}
	return e.regex.MatchString(strVal)
}

func (e *conditionEval) numericCompare(value any) (int, error) {
	v1, err := condToFloat64(value)
	if err != nil {
		return 0, err
	}
	v2, err := condToFloat64(e.cond.Value)
	if err != nil {
		return 0, err
	}

	if v1 < v2 {
		return -1, nil
	} else if v1 > v2 {
		return 1, nil
	}
	return 0, nil
}

func (e *conditionEval) isIn(value any) bool {
	condVal := reflect.ValueOf(e.cond.Value)
	if condVal.Kind() != reflect.Slice {
		return false
	}
	for i := 0; i < condVal.Len(); i++ {
		if reflect.DeepEqual(value, condVal.Index(i).Interface()) {
			return true
		}
	}
	return false
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

// evalContext provides a lazily-parsed JSON payload cache shared across
// all condition evaluations for a single envelope. This avoids parsing
// the payload N*M times (N rules x M conditions).
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
