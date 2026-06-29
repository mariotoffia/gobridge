package shared

import "reflect"

// secretType is the reflect.Type of Secret, matched structurally so the reveal
// walk can substitute a revealed copy wherever a Secret leaf appears.
//
//nolint:gochecknoglobals // immutable reflect.Type computed once; Go has no const reflect.Type
var secretType = reflect.TypeOf(Secret{})

// RevealSecrets returns a deep copy of v in which every Secret value (anywhere
// in the reachable struct / slice / array / map / pointer / interface graph) is
// replaced by its Revealed() form, so a subsequent json.Marshal / yaml.Marshal
// of the result emits the REAL secret value instead of "[REDACTED]".
//
// This is the value-carried persistence escape hatch: the reveal intent lives
// on the returned copy only, so the input is never mutated and concurrent
// default (redacting) marshalling elsewhere is unaffected. It is the single
// approved way for the authoritative config/credential serializers (config
// save, deploy-config secret scanner) to obtain a revealed view for marshalling.
//
// Subtrees containing no Secret are returned by reference (cheap no-op): a copy
// is allocated only along paths that actually carry a Secret.
func RevealSecrets(v any) any {
	if v == nil {
		return nil
	}
	out := revealValue(reflect.ValueOf(v))
	if !out.IsValid() {
		return v
	}
	return out.Interface()
}

// revealValue returns a value equal to rv with Secret leaves revealed, or an
// invalid Value when rv contains no Secret (signalling "reuse the original").
func revealValue(rv reflect.Value) reflect.Value {
	if !rv.IsValid() {
		return reflect.Value{}
	}

	t := rv.Type()
	if t == secretType {
		s, _ := rv.Interface().(Secret)
		return reflect.ValueOf(s.Revealed())
	}
	if !typeHasSecret(t, map[reflect.Type]bool{}) {
		return reflect.Value{}
	}

	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return reflect.Value{}
		}
		elem := revealValue(rv.Elem())
		if !elem.IsValid() {
			return reflect.Value{}
		}
		p := reflect.New(rv.Elem().Type())
		p.Elem().Set(elem)
		return p

	case reflect.Interface:
		if rv.IsNil() {
			return reflect.Value{}
		}
		elem := revealValue(rv.Elem())
		if !elem.IsValid() {
			return reflect.Value{}
		}
		out := reflect.New(t).Elem()
		out.Set(elem)
		return out

	case reflect.Struct:
		out := reflect.New(t).Elem()
		out.Set(rv)
		changed := false
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).PkgPath != "" {
				continue // unexported (the Secret leaf is handled above)
			}
			if fv := revealValue(rv.Field(i)); fv.IsValid() {
				out.Field(i).Set(fv)
				changed = true
			}
		}
		if !changed {
			return reflect.Value{}
		}
		return out

	case reflect.Slice:
		if rv.IsNil() {
			return reflect.Value{}
		}
		return revealIndexable(rv, reflect.MakeSlice(t, rv.Len(), rv.Len()))

	case reflect.Array:
		return revealIndexable(rv, reflect.New(t).Elem())

	case reflect.Map:
		if rv.IsNil() {
			return reflect.Value{}
		}
		out := reflect.MakeMapWithSize(t, rv.Len())
		changed := false
		iter := rv.MapRange()
		for iter.Next() {
			val := iter.Value()
			if nv := revealValue(val); nv.IsValid() {
				out.SetMapIndex(iter.Key(), nv)
				changed = true
			} else {
				out.SetMapIndex(iter.Key(), val)
			}
		}
		if !changed {
			return reflect.Value{}
		}
		return out

	default:
		return reflect.Value{}
	}
}

// revealIndexable copies each element of rv into out, revealing Secret leaves.
func revealIndexable(rv, out reflect.Value) reflect.Value {
	changed := false
	for i := 0; i < rv.Len(); i++ {
		if ev := revealValue(rv.Index(i)); ev.IsValid() {
			out.Index(i).Set(ev)
			changed = true
		} else {
			out.Index(i).Set(rv.Index(i))
		}
	}
	if !changed {
		return reflect.Value{}
	}
	return out
}

// typeHasSecret reports whether t (or anything reachable through it) is or
// contains a Secret, so revealValue can short-circuit secret-free subtrees.
// visited guards recursive types.
func typeHasSecret(t reflect.Type, visited map[reflect.Type]bool) bool {
	if t == secretType {
		return true
	}
	if visited[t] {
		return false
	}
	visited[t] = true

	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		return typeHasSecret(t.Elem(), visited)
	case reflect.Interface:
		return true // dynamic type unknown; inspect at runtime
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" && f.Type != secretType {
				continue
			}
			if typeHasSecret(f.Type, visited) {
				return true
			}
		}
	}
	return false
}
