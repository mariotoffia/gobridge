package transform

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// setNestedValue sets a value at a nested path in a map.
// Path is dot-separated, e.g., "user.profile.name"
func setNestedValue(data map[string]any, path string, value any) {
	parts := strings.Split(path, ".")

	current := data
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]
		if next, ok := current[part].(map[string]any); ok {
			current = next
		} else {
			// Create nested map
			next := make(map[string]any)
			current[part] = next
			current = next
		}
	}

	current[parts[len(parts)-1]] = value
}

// applyTransform applies a transformation to a value.
func applyTransform(value any, transform TransformType) (any, error) {
	switch transform {
	case TransformNone:
		return value, nil

	case TransformString:
		return fmt.Sprintf("%v", value), nil

	case TransformInt:
		return toInt(value)

	case TransformFloat:
		return toFloat(value)

	case TransformBool:
		return toBool(value)

	case TransformBase64Encode:
		str := fmt.Sprintf("%v", value)
		return base64.StdEncoding.EncodeToString([]byte(str)), nil

	case TransformBase64Decode:
		str, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("base64decode requires string input")
		}
		decoded, err := base64.StdEncoding.DecodeString(str)
		if err != nil {
			return nil, fmt.Errorf("invalid base64: %w", err)
		}
		return string(decoded), nil

	default:
		return nil, fmt.Errorf("unsupported transform: %s", transform)
	}
}

// toInt converts a value to int64.
func toInt(v any) (int64, error) {
	switch val := v.(type) {
	case int:
		return int64(val), nil
	case int64:
		return val, nil
	case int32:
		return int64(val), nil
	case float64:
		return int64(val), nil
	case float32:
		return int64(val), nil
	case string:
		i, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse int %q: %w", val, err)
		}
		return i, nil
	case bool:
		if val {
			return 1, nil
		}
		return 0, nil
	default:
		return 0, fmt.Errorf("cannot convert %T to int", v)
	}
}

// toFloat converts a value to float64.
func toFloat(v any) (float64, error) {
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
	default:
		return 0, fmt.Errorf("cannot convert %T to float", v)
	}
}

// toBool converts a value to bool.
func toBool(v any) (bool, error) {
	switch val := v.(type) {
	case bool:
		return val, nil
	case int:
		return val != 0, nil
	case int64:
		return val != 0, nil
	case int32:
		return val != 0, nil
	case float64:
		return val != 0, nil
	case float32:
		return val != 0, nil
	case string:
		b, err := strconv.ParseBool(val)
		if err != nil {
			return false, fmt.Errorf("parse bool %q: %w", val, err)
		}
		return b, nil
	default:
		return false, fmt.Errorf("cannot convert %T to bool", v)
	}
}
