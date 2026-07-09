package transform

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// headerTargetPrefix marks a FieldMapping.Target that writes to an
// envelope header rather than the payload object.
const headerTargetPrefix = "header."

// headerTarget reports whether target addresses an envelope header and,
// if so, returns the header key (the portion after "header."). A bare
// "header." prefix with no key returns ("", true) so callers can reject
// it as an invalid configuration.
func headerTarget(target string) (key string, ok bool) {
	if !strings.HasPrefix(target, headerTargetPrefix) {
		return "", false
	}
	return strings.TrimPrefix(target, headerTargetPrefix), true
}

// setNestedValue sets value at a dot-separated path in data, creating
// intermediate maps as needed. It returns an error when a path segment
// crosses an EXISTING non-map (scalar/array) value: writing through it would
// silently replace that value with a map and discard it. An absent segment is
// created; only a present-but-non-object segment is a conflict.
func setNestedValue(data map[string]any, path string, value any) error {
	parts := strings.Split(path, ".")

	current := data
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]
		existing, present := current[part]
		if !present {
			next := make(map[string]any)
			current[part] = next
			current = next
			continue
		}
		next, ok := existing.(map[string]any)
		if !ok {
			return fmt.Errorf("target path %q crosses non-object value at %q",
				path, strings.Join(parts[:i+1], "."))
		}
		current = next
	}

	current[parts[len(parts)-1]] = value
	return nil
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

// validTransformType reports whether t is a supported TransformType.
// Checked at construction: with the default FailOnError=false an
// unknown type would otherwise silently skip its mapping per message.
func validTransformType(t TransformType) bool {
	switch t {
	case TransformNone, TransformString, TransformInt, TransformFloat,
		TransformBool, TransformBase64Encode, TransformBase64Decode:
		return true
	default:
		return false
	}
}

// deepCopyMap deep-copies a parsed JSON object so payload mappings can
// write into the copy while every read still sees the pristine source
// (no aliasing between mapping chain steps).
func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return deepCopyMap(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopyValue(e)
		}
		return out
	default:
		return x
	}
}

// marshalPayload serializes a transformed payload WITHOUT HTML escaping:
// the result is a message body, not HTML, and escaping <, > and & would
// corrupt byte-sensitive consumers. encoding/json's Encoder appends a
// trailing newline, which is trimmed.
func marshalPayload(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("marshal transformed payload: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// redactValue returns a content-free descriptor of a payload value for use
// in error messages: it reveals only the value's TYPE and, for
// length-bearing kinds, its LENGTH — never the value's contents, which may
// carry secrets or PII. Conversion errors flow into route logs, so echoing
// the raw failed value (or a strconv error that embeds it) would leak the
// payload. Callers add the field PATH and target type from the mapping.
func redactValue(v any) string {
	switch x := v.(type) {
	case string:
		return fmt.Sprintf("type=string len=%d", len(x))
	case []byte:
		return fmt.Sprintf("type=[]byte len=%d", len(x))
	case []any:
		return fmt.Sprintf("type=array len=%d", len(x))
	case map[string]any:
		return fmt.Sprintf("type=object len=%d", len(x))
	default:
		return fmt.Sprintf("type=%T", v)
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
		return floatToInt64(val)
	case float32:
		return floatToInt64(float64(val))
	case string:
		i, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			// Redact: neither the raw value nor strconv's error (which
			// embeds the raw value) may reach the log.
			return 0, fmt.Errorf("cannot parse %s as int", redactValue(val))
		}
		return i, nil
	case bool:
		if val {
			return 1, nil
		}
		return 0, nil
	default:
		return 0, fmt.Errorf("cannot convert %s to int", redactValue(v))
	}
}

// floatToInt64 converts a finite, in-range float to int64. It rejects NaN and
// ±Inf, and magnitudes outside the int64 range: a bare int64(val) on those is
// implementation-defined (typically a silent MinInt64), turning e.g. 1e300 into
// garbage instead of an error. float64(math.MaxInt64) rounds up to 2^63, so the
// upper bound uses >= to exclude that unrepresentable boundary value. Error
// messages state the TYPE only — the numeric magnitude is payload content and
// is never echoed.
func floatToInt64(val float64) (int64, error) {
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return 0, fmt.Errorf("cannot convert non-finite float64 to int64")
	}
	if val >= float64(math.MaxInt64) || val < float64(math.MinInt64) {
		return 0, fmt.Errorf("float64 value out of int64 range")
	}
	return int64(val), nil
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
			// Redact: strconv's error embeds the raw value.
			return 0, fmt.Errorf("cannot parse %s as float", redactValue(val))
		}
		return f, nil
	default:
		return 0, fmt.Errorf("cannot convert %s to float", redactValue(v))
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
			// Redact: strconv's error embeds the raw value.
			return false, fmt.Errorf("cannot parse %s as bool", redactValue(val))
		}
		return b, nil
	default:
		return false, fmt.Errorf("cannot convert %s to bool", redactValue(v))
	}
}
