package filter

import (
	"encoding/json"
	"fmt"
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

	if c.Operator == OperatorRegex {
		pattern, ok := c.Value.(string)
		if !ok {
			return nil, fmt.Errorf("regex operator requires string value")
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regex pattern: %w", err)
		}
		eval.regex = re
	}

	return eval, nil
}

func (e *conditionEvaluator) evaluate(env *messaging.Envelope) (bool, error) {
	value, exists, err := e.extractField(env)
	if err != nil {
		return false, err
	}

	if e.condition.Operator == OperatorExists {
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

func (e *conditionEvaluator) extractField(env *messaging.Envelope) (any, bool, error) {
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
		return e.extractFromPayload(env.Payload(), field)
	default:
		if env.Headers() == nil {
			return nil, false, nil
		}
		val, ok := env.Headers()[field]
		return val, ok, nil
	}
}

func (e *conditionEvaluator) extractFromPayload(payload []byte, path string) (any, bool, error) {
	if len(payload) == 0 {
		return nil, false, nil
	}

	if e.maxPayloadBytes > 0 && len(payload) > e.maxPayloadBytes {
		return nil, false, shared.ErrPayloadTooLarge.Wrap(
			fmt.Errorf("filter: payload %d bytes exceeds limit %d", len(payload), e.maxPayloadBytes))
	}

	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		// SECURITY: a JSON parse failure must NOT be a silent no-match.
		// Treating malformed JSON as "field absent" lets a hostile
		// producer bypass an ActionDrop (deny) rule by sending garbage.
		// Fail closed: classify the payload as rejected so the runtime
		// DLQs it instead of passing it down the chain.
		return nil, false, shared.ErrInvalidPayload.Wrap(
			fmt.Errorf("filter: %q path on unparseable JSON payload: %w", path, err))
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
	return reflect.DeepEqual(value, e.condition.Value)
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
		return 0, err
	}
	v2, err := toFloat64(e.condition.Value)
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

func (e *conditionEvaluator) isIn(value any) bool {
	condVal := reflect.ValueOf(e.condition.Value)
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
