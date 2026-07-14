package bridge

import (
	"context"
	"fmt"
	"reflect"

	"github.com/mariotoffia/gobridge/ports"
)

func (b *Builder) resolveProcessors(names []string) ([]ports.Processor, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]ports.Processor, 0, len(names))
	for _, n := range names {
		p, ok := b.processors[n]
		if !ok {
			return nil, fmt.Errorf("bridge: unknown processor %q", n)
		}
		out = append(out, p)
	}
	return out, nil
}

// resolveConfigCredentials inspects cfg for the optional
// CredentialedConfig contract, resolves the URI through the configured
// credential store, and applies the resolved material in place. It
// returns the URI (so callers can register credential-refresh
// watchers) and any error from store lookup or apply.
func (b *Builder) resolveConfigCredentials(ctx context.Context, cfg ports.PluginConfig, label string) (string, error) {
	cc, ok := cfg.(ports.CredentialedConfig)
	if !ok {
		return "", nil
	}
	uri := cc.CredentialsURI()
	if uri == "" {
		return "", nil
	}
	if b.credStore == nil {
		return "", fmt.Errorf("bridge: %s: credentials_uri specified but no credential store registered", label)
	}
	creds, err := b.credStore.Resolve(ctx, uri)
	if err != nil {
		return "", fmt.Errorf("bridge: %s: resolve credentials: %w", label, err)
	}
	if err := cc.ApplyCredentials(creds); err != nil {
		return "", fmt.Errorf("bridge: %s: apply credentials: %w", label, err)
	}
	return uri, nil
}

// cloneConfigForBuild deep-clones a blueprint before validation or build. It
// freezes every exported pointer, interface, slice, and map recursively,
// including mutable collections nested inside adapter-owned PluginConfig
// values, without importing adapter types. Unexported fields are retained by a
// shallow struct copy; this deliberately preserves private process-stable
// identity state such as a cached client-ID suffix.
func cloneConfigForBuild(cfg *ports.BridgeConfig) *ports.BridgeConfig {
	if cfg == nil {
		return nil
	}
	cloned := deepCloneConfigValue(reflect.ValueOf(cfg), make(map[cloneVisit]reflect.Value))
	return cloned.Interface().(*ports.BridgeConfig)
}

type cloneVisit struct {
	typeOf reflect.Type
	kind   reflect.Kind
	ptr    uintptr
}

func deepCloneConfigValue(value reflect.Value, seen map[cloneVisit]reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}

	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := deepCloneConfigValue(value.Elem(), seen)
		out := reflect.New(value.Type()).Elem()
		out.Set(cloned)
		return out
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		visit := cloneVisit{typeOf: value.Type(), kind: value.Kind(), ptr: value.Pointer()}
		if prior, ok := seen[visit]; ok {
			return prior
		}
		out := reflect.New(value.Type().Elem())
		seen[visit] = out
		out.Elem().Set(deepCloneConfigValue(value.Elem(), seen))
		return out
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		visit := cloneVisit{typeOf: value.Type(), kind: value.Kind(), ptr: value.Pointer()}
		if visit.ptr != 0 {
			if prior, ok := seen[visit]; ok {
				return prior
			}
		}
		out := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		if visit.ptr != 0 {
			seen[visit] = out
		}
		for i := range value.Len() {
			out.Index(i).Set(deepCloneConfigValue(value.Index(i), seen))
		}
		return out
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		visit := cloneVisit{typeOf: value.Type(), kind: value.Kind(), ptr: value.Pointer()}
		if prior, ok := seen[visit]; ok {
			return prior
		}
		out := reflect.MakeMapWithSize(value.Type(), value.Len())
		seen[visit] = out
		iter := value.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), deepCloneConfigValue(iter.Value(), seen))
		}
		return out
	case reflect.Struct:
		out := reflect.New(value.Type()).Elem()
		out.Set(value)
		for i := range value.NumField() {
			// Reflect cannot safely traverse an unexported field from another
			// package. The whole-struct Set above already preserved its value.
			if value.Type().Field(i).PkgPath != "" {
				continue
			}
			out.Field(i).Set(deepCloneConfigValue(value.Field(i), seen))
		}
		return out
	case reflect.Array:
		out := reflect.New(value.Type()).Elem()
		for i := range value.Len() {
			out.Index(i).Set(deepCloneConfigValue(value.Index(i), seen))
		}
		return out
	default:
		return value
	}
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
