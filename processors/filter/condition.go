package filter

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/mariotoffia/gobridge/domain"
)

type conditionEvaluator struct {
	condition Condition
	regex     *regexp.Regexp
}

func newConditionEvaluator(c Condition) (*conditionEvaluator, error) {
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

func (e *conditionEvaluator) evaluate(env *domain.Envelope) (bool, error) {
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

func (e *conditionEvaluator) extractField(env *domain.Envelope) (any, bool, error) {
	field := e.condition.Field

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
		return e.extractFromPayload(env.Payload, field)
	default:
		if env.Headers == nil {
			return nil, false, nil
		}
		val, ok := env.Headers[field]
		return val, ok, nil
	}
}

func (e *conditionEvaluator) extractFromPayload(payload []byte, path string) (any, bool, error) {
	if len(payload) == 0 {
		return nil, false, nil
	}

	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
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
		return false, fmt.Errorf("unsupported operator: %s", e.condition.Operator)
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
		return strconv.ParseFloat(val, 64)
	case json.Number:
		return val.Float64()
	default:
		return 0, fmt.Errorf("cannot convert %T to float64", v)
	}
}
