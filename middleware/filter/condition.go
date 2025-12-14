package filter

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// conditionEvaluator evaluates a single condition against a message.
type conditionEvaluator struct {
	condition Condition
	regex     *regexp.Regexp // cached regex for regex operator
}

// newConditionEvaluator creates a new condition evaluator.
func newConditionEvaluator(c Condition) (*conditionEvaluator, error) {
	eval := &conditionEvaluator{condition: c}

	// Pre-compile regex if needed
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

// evaluate checks if the condition matches the message.
func (e *conditionEvaluator) evaluate(msg *types.Message) (bool, error) {
	// Extract the field value
	value, exists, err := e.extractField(msg)
	if err != nil {
		return false, err
	}

	// Handle exists operator
	if e.condition.Operator == OperatorExists {
		expected, _ := e.condition.Value.(bool)
		if !expected {
			return !exists, nil
		}
		return exists, nil
	}

	// If field doesn't exist, condition fails
	if !exists {
		return false, nil
	}

	// Evaluate based on operator
	return e.compare(value)
}

// extractField extracts the field value from the message.
func (e *conditionEvaluator) extractField(msg *types.Message) (any, bool, error) {
	field := e.condition.Field

	switch {
	case field == "topic":
		return msg.Topic, true, nil

	case strings.HasPrefix(field, "metadata."):
		key := strings.TrimPrefix(field, "metadata.")
		if msg.Metadata == nil {
			return nil, false, nil
		}
		val, ok := msg.Metadata[key]
		return val, ok, nil

	case strings.HasPrefix(field, "$."):
		// Simple JSONPath - just extract from payload
		return e.extractFromPayload(msg.Payload, field)

	default:
		// Treat as direct field name in metadata
		if msg.Metadata == nil {
			return nil, false, nil
		}
		val, ok := msg.Metadata[field]
		return val, ok, nil
	}
}

// extractFromPayload extracts a value from the JSON payload using a simple path.
func (e *conditionEvaluator) extractFromPayload(payload []byte, path string) (any, bool, error) {
	if len(payload) == 0 {
		return nil, false, nil
	}

	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		return nil, false, nil // Not JSON or invalid
	}

	// Strip $. prefix
	path = strings.TrimPrefix(path, "$.")

	// Split path and traverse
	parts := strings.Split(path, ".")
	var current any = data

	for _, part := range parts {
		switch v := current.(type) {
		case map[string]any:
			val, ok := v[part]
			if !ok {
				return nil, false, nil
			}
			current = val
		default:
			return nil, false, nil
		}
	}

	return current, true, nil
}

// compare compares the extracted value against the condition value.
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

// equals checks if two values are equal.
func (e *conditionEvaluator) equals(value any) bool {
	return reflect.DeepEqual(value, e.condition.Value)
}

// contains checks if the value contains the condition value (string).
func (e *conditionEvaluator) contains(value any) bool {
	strVal := fmt.Sprintf("%v", value)
	strCond := fmt.Sprintf("%v", e.condition.Value)
	return strings.Contains(strVal, strCond)
}

// matchesRegex checks if the value matches the regex pattern.
func (e *conditionEvaluator) matchesRegex(value any) bool {
	if e.regex == nil {
		return false
	}
	strVal := fmt.Sprintf("%v", value)
	return e.regex.MatchString(strVal)
}

// numericCompare compares two values numerically.
// Returns: -1 if value < condition, 0 if equal, 1 if value > condition
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

// isIn checks if the value is in the condition value (slice).
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

// toFloat64 converts a value to float64.
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
