package parser

import (
	"reflect"
	"strings"
	"time"
)

// Why the JSON projection writes a duration as a string.
//
// JSON has no duration literal, and the config decoder deliberately refuses a
// bare number for a duration field: `timeout: 30` meaning thirty NANOSECONDS is
// a footgun, so the decoder insists on `"30s"`. `encoding/json` writes a
// `time.Duration` as exactly that bare number, so any plugin config carrying one
// projected to JSON could not be read back — and `MarshalBridgeConfigJSON`
// documents itself as the ROUND-TRIPPABLE wire form.
//
// What that cost: a coordinated cohort records the config it committed as a
// durable artifact through this projection, and a member that restarts while its
// own config source holds a candidate the cohort has not committed recovers from
// that artifact rather than running a generation no peer runs. With a duration
// anywhere in the config — a store's `compaction_grace`, a broker's
// `connect_timeout`, both of which the shipped AWS profile sets — the artifact
// could not be decoded, so the member refused to start and stayed down. The YAML
// projection never had the problem: `yaml.v3` writes a `time.Duration` in the
// form the decoder reads.
//
// So the JSON projection writes every duration the way the decoder reads one.

// jsonDurationsAsStrings rewrites, in place, every entry of opts that came from a
// time.Duration field of value, replacing the numeric projection with the string
// form the decoder accepts. It walks nested structs alongside the nested maps
// they produced, and follows an embedded struct into the SAME map because that is
// where encoding/json flattens it.
//
// A field the projection did not emit (omitempty, or a name the caller renamed)
// is left alone: the walk only ever rewrites a key that is already there.
func jsonDurationsAsStrings(value any, opts map[string]any) {
	if opts == nil {
		return
	}
	structValue, ok := structOf(value)
	if !ok {
		return
	}
	structType := structValue.Type()
	for i := range structType.NumField() {
		field := structType.Field(i)
		if !field.IsExported() {
			continue
		}
		name, emitted := jsonFieldName(field)
		if !emitted {
			continue
		}
		fieldValue := structValue.Field(i)
		if name == "" {
			// An embedded struct with no json tag: encoding/json promotes its
			// fields into the parent object, so it shares the parent's map.
			jsonDurationsAsStrings(fieldValue.Interface(), opts)
			continue
		}
		if fieldValue.Type() == reflect.TypeFor[time.Duration]() {
			if _, present := opts[name]; present {
				opts[name] = time.Duration(fieldValue.Int()).String()
			}
			continue
		}
		if nested, isObject := opts[name].(map[string]any); isObject {
			jsonDurationsAsStrings(fieldValue.Interface(), nested)
		}
	}
}

// structOf dereferences pointers and interfaces down to a struct value, and
// reports false for anything that is not one (or is nil on the way).
func structOf(value any) (reflect.Value, bool) {
	v := reflect.ValueOf(value)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return reflect.Value{}, false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}
	return v, true
}

// jsonFieldName returns the object key encoding/json emits for field, and false
// when the field is not emitted at all. An embedded struct that carries no name
// returns ("", true) — the caller reads that as "flattened into my own map".
func jsonFieldName(field reflect.StructField) (string, bool) {
	tag, tagged := field.Tag.Lookup("json")
	name, _, _ := strings.Cut(tag, ",")
	switch {
	case tagged && name == "-" && tag == "-":
		return "", false
	case name != "":
		return name, true
	case field.Anonymous:
		return "", true
	default:
		return field.Name, true
	}
}
