package parser

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/go-viper/mapstructure/v2"

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
			floatToIntegerOrDurationHook,
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
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
		return fmt.Errorf("config: decode plugin options: %w", err)
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

// floatToIntegerOrDurationHook rejects YAML/JSON float inputs that
// would silently lose information when coerced into an integer or a
// time.Duration. JSON unmarshalling produces float64 for any numeric
// literal, so without this hook `maxMessages: 5.9` would truncate to
// 5 and `flushInterval: 30` would be interpreted as 30 nanoseconds.
//
//   - float -> int*/uint*: error if the value has a fractional part;
//     otherwise pass through and let mapstructure's built-in
//     coercion narrow it to the integer target.
//   - float -> time.Duration: always error. Durations must be given
//     as a string literal (e.g. "30s") to avoid the
//     nanoseconds-vs-seconds ambiguity.
//   - any other (from, to) pair: pass through unchanged.
func floatToIntegerOrDurationHook(from reflect.Type, to reflect.Type, data any) (any, error) {
	if from == nil || to == nil {
		return data, nil
	}
	switch from.Kind() {
	case reflect.Float32, reflect.Float64:
	default:
		return data, nil
	}

	var f float64
	switch v := data.(type) {
	case float32:
		f = float64(v)
	case float64:
		f = v
	default:
		return data, nil
	}

	if to == reflect.TypeOf(time.Duration(0)) {
		return nil, fmt.Errorf("cannot decode bare number %v into time.Duration; use a string like \"30s\"", data)
	}

	switch to.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if math.Trunc(f) != f {
			return nil, fmt.Errorf("cannot decode fractional float %v into %s field", data, to.Kind())
		}
		return data, nil
	}

	return data, nil
}
