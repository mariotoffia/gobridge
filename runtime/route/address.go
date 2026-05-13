package route

import (
	"fmt"
	"strings"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// RenderAddress replaces {key} placeholders in template with values from vars.
// Returns an error if a placeholder references a missing key or if the rendered
// result is empty. Substituted values are never re-expanded, preventing
// infinite loops and header-value injection.
func RenderAddress(template string, vars map[string]any) (string, error) {
	if template == "" {
		return "", nil
	}

	var b strings.Builder
	remaining := template
	for remaining != "" {
		start := strings.Index(remaining, "{")
		if start < 0 {
			b.WriteString(remaining)
			break
		}
		end := strings.Index(remaining[start:], "}")
		if end < 0 {
			b.WriteString(remaining)
			break
		}
		end += start

		key := remaining[start+1 : end]
		if key == "" {
			return "", fmt.Errorf("empty placeholder in address template %q", template)
		}

		val, ok := messaging.GetHeaderString(vars, key)
		if !ok {
			return "", fmt.Errorf("address template placeholder {%s} not found in headers", key)
		}

		b.WriteString(remaining[:start])
		b.WriteString(val)
		remaining = remaining[end+1:]
	}

	result := b.String()
	if result == "" {
		return "", fmt.Errorf("address template %q rendered to empty string", template)
	}

	return result, nil
}

// CopyHeaders returns a deep copy of opts suitable for stamping onto a
// cloned envelope. Nested map[string]any, []any, []string and []byte
// values are duplicated so mutations on the copy never bleed back into
// the source map. A nil or empty input returns nil.
func CopyHeaders(opts map[string]any) map[string]any {
	if len(opts) == 0 {
		return nil
	}
	cp := make(map[string]any, len(opts))
	for k, v := range opts {
		cp[k] = deepCopyHeaderValue(v)
	}
	return cp
}

func deepCopyHeaderValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		cp := make(map[string]any, len(val))
		for k, v := range val {
			cp[k] = deepCopyHeaderValue(v)
		}
		return cp
	case []any:
		s := make([]any, len(val))
		for i, elem := range val {
			s[i] = deepCopyHeaderValue(elem)
		}
		return s
	case []string:
		s := make([]string, len(val))
		copy(s, val)
		return s
	case []byte:
		if val == nil {
			return val
		}
		s := make([]byte, len(val))
		copy(s, val)
		return s
	default:
		return v
	}
}
