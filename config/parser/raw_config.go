package parser

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/go-viper/mapstructure/v2"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// rawMapConfig is the concrete ports.RawConfig produced by stage 1 of
// the two-stage parser. It carries a yaml-decoded map[string]any (the
// `options:` block of one plugin attachment point) and decodes it
// into the adapter's typed Config struct in stage 2.
type rawMapConfig struct {
	data map[string]any
}

// NewRawConfig wraps a stage-1 map[string]any as a ports.RawConfig. A
// nil or empty map is permitted: Decode against it leaves the target
// at its zero value, matching the behaviour of an absent `options:`
// block in YAML.
func NewRawConfig(data map[string]any) ports.RawConfig {
	return &rawMapConfig{data: data}
}

// Decode populates target from the wrapped map. target must be a
// non-nil pointer to a struct.
//
// Tag convention: Decode honours `json:"..."` tags on the target
// struct. The repo-wide convention for adapter Config structs is
// camelCase JSON tags (e.g. `serviceName`, `flushInterval`); using
// the json tag here keeps a single source of truth for option naming.
//
// Strictness: Decode is strict and all-or-nothing. Any unknown key
// (typo, removed field, wrong shape) fails the entire decode and the
// target is reset to its zero value before the error is returned.
// Type mismatches (e.g. a string where an int is expected) are
// likewise surfaced as errors — WeaklyTypedInput is disabled.
//
// Convenience hooks: textual durations such as `"30s"` decode into
// time.Duration, and comma-separated strings decode into []string —
// both convenient when an option originates from an environment
// variable or a flat YAML scalar.
func (r *rawMapConfig) Decode(target any) error {
	if target == nil {
		return errors.New("config: RawConfig.Decode target is nil")
	}
	if r == nil || len(r.data) == 0 {
		// Nothing to decode — leave target at its zero value. The
		// adapter's Validate() is the right place to reject empty
		// configs when fields are required.
		return nil
	}

	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		TagName:          "json",
		ErrorUnused:      true,
		WeaklyTypedInput: false,
		Result:           target,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			numericToIntegerOrDurationHook,
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
			// Decodes a scalar string into any value-object that
			// implements encoding.TextUnmarshaler — notably
			// shared.Secret on plugin-config secret fields (mqtt
			// session.password, servicebus connection_string /
			// client_secret, http api_key). mapstructure does not
			// honour TextUnmarshaler natively, so without this hook a
			// `password: <scalar>` fails with "expected a map or
			// struct, got string". Placed last so the numeric/duration/
			// slice hooks keep precedence.
			mapstructure.TextUnmarshallerHookFunc(),
		),
	})
	if err != nil {
		return fmt.Errorf("config: build plugin options decoder: %w", err)
	}

	if err := dec.Decode(r.data); err != nil {
		// Strict decode is all-or-nothing: discard any partial
		// population of target before surfacing the error.
		if v := reflect.ValueOf(target); v.Kind() == reflect.Pointer && !v.IsNil() {
			elem := v.Elem()
			if elem.CanSet() {
				elem.Set(reflect.Zero(elem.Type()))
			}
		}
		return shared.ErrInvalidConfig.Wrap(err).WithMessage(
			"config: decode plugin options: " + err.Error(),
		)
	}
	return nil
}

// AsMap returns the underlying option map for diagnostic peeks (e.g.
// the config model's stale_claim_duration check). It is the satisfier
// for the unexported rawMapView interface used in package config;
// alternative ports.RawConfig implementations may return nil. The
// returned map is the live underlying storage and MUST NOT be mutated
// by callers.
func (r *rawMapConfig) AsMap() map[string]any {
	if r == nil {
		return nil
	}
	return r.data
}

// numericToIntegerOrDurationHook rejects numeric inputs that would silently
// lose information or be misread when coerced into an integer or a
// time.Duration.
//
// JSON unmarshalling produces float64 for every numeric literal, and YAML
// produces a Go int for a bare integer literal. Either way, a bare number
// decoded into a time.Duration is interpreted as NANOSECONDS, so
// `heartbeat: 30` silently becomes 30ns instead of the 30s the operator
// meant. This hook forbids that entirely.
//
//   - number -> time.Duration: always error (both float and integer sources).
//     Durations must be given as a string literal with a unit (e.g. "30s").
//   - number -> int*/uint*: error if the value is fractional, negative for an
//     unsigned target, or outside the exact target width. Validation happens
//     before mapstructure narrows the value, preventing wraparound.
//   - any other (from, to) pair: pass through unchanged.
func numericToIntegerOrDurationHook(from reflect.Type, to reflect.Type, data any) (any, error) {
	if from == nil || to == nil {
		return data, nil
	}

	// Reject ANY bare number into a duration field regardless of whether the
	// source decoded as a float (JSON) or an integer (YAML bare int). A bare
	// number would be read as nanoseconds — never what an operator intends.
	// A value that is ALREADY a time.Duration (from == Duration) is explicit
	// and passes through unchanged.
	durationType := reflect.TypeOf(time.Duration(0))
	if to == durationType && from != durationType {
		switch from.Kind() {
		case reflect.Float32, reflect.Float64,
			reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return nil, fmt.Errorf("cannot decode bare number %v into a duration field; use a "+
				"string with a unit like \"30s\" (a bare number is interpreted as nanoseconds)", data)
		default:
			return data, nil
		}
	}

	if !isIntegerKind(to.Kind()) {
		return data, nil
	}
	targetBits := to.Bits()
	switch from.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value := reflect.ValueOf(data).Int()
		if isUnsignedIntegerKind(to.Kind()) {
			if value < 0 {
				return nil, fmt.Errorf("cannot decode negative integer %d into %s", value, to)
			}
			if targetBits < 64 && uint64(value) >= uint64(1)<<targetBits {
				return nil, fmt.Errorf("integer %d is out of range for %s", value, to)
			}
			return data, nil
		}
		if targetBits < 64 {
			minimum := -int64(1 << (targetBits - 1))
			maximum := int64(1<<(targetBits-1)) - 1
			if value < minimum || value > maximum {
				return nil, fmt.Errorf("integer %d is out of range for %s", value, to)
			}
		}
		return data, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value := reflect.ValueOf(data).Uint()
		if isUnsignedIntegerKind(to.Kind()) {
			if targetBits < 64 && value >= uint64(1)<<targetBits {
				return nil, fmt.Errorf("integer %d is out of range for %s", value, to)
			}
			return data, nil
		}
		var maximum uint64 = math.MaxInt64
		if targetBits < 64 {
			maximum = uint64(1<<(targetBits-1)) - 1
		}
		if value > maximum {
			return nil, fmt.Errorf("integer %d is out of range for %s", value, to)
		}
		return data, nil
	case reflect.Float32, reflect.Float64:
		value := reflect.ValueOf(data).Float()
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("cannot decode non-finite float %v into %s", data, to)
		}
		if math.Trunc(value) != value {
			return nil, fmt.Errorf("cannot decode fractional float %v into %s field", data, to.Kind())
		}
		if isUnsignedIntegerKind(to.Kind()) {
			upperExclusive := math.Ldexp(1, targetBits)
			if value < 0 || value >= upperExclusive {
				return nil, fmt.Errorf("float %v is out of range for %s", data, to)
			}
			return data, nil
		}
		lowerInclusive := -math.Ldexp(1, targetBits-1)
		upperExclusive := math.Ldexp(1, targetBits-1)
		if value < lowerInclusive || value >= upperExclusive {
			return nil, fmt.Errorf("float %v is out of range for %s", data, to)
		}
		return data, nil
	default:
		return data, nil
	}
}

func isIntegerKind(kind reflect.Kind) bool {
	switch kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	default:
		return false
	}
}

func isUnsignedIntegerKind(kind reflect.Kind) bool {
	switch kind {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	default:
		return false
	}
}
