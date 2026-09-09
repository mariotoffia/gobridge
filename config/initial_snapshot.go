package config

import (
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// initialSnapshot clones blueprint-owned state without a serialization round
// trip (which could redact secrets or change numeric condition types). Opaque
// plugin state is copied only by its adapter's FreezableConfig capability.
func initialSnapshot(cfg *ports.BridgeConfig) (*ports.BridgeConfig, error) {
	if cfg == nil {
		return nil, shared.ErrInvalidConfig.WithMessage("config initialization: nil blueprint")
	}
	out, err := DefaultMerge(cfg, &ports.BridgeConfig{})
	if err != nil {
		return nil, err
	}
	if out.Bridge.Cluster != nil {
		out.Bridge.Cluster.Members = slices.Clone(out.Bridge.Cluster.Members)
	}
	var plugins []*ports.PluginConfig
	for _, store := range []*ports.StoreConfig{out.Stores.Lease, out.Stores.Outbox, out.Stores.DLQ, out.Stores.ManagedSubscriptions} {
		if store != nil {
			plugins = append(plugins, &store.Config)
		}
	}
	for i := range out.Sessions {
		plugins = append(plugins, &out.Sessions[i].Config)
	}
	for i := range out.Receivers {
		receiver := &out.Receivers[i]
		receiver.Topics = slices.Clone(receiver.Topics)
		plugins = append(plugins, &receiver.Config)
		for j := range receiver.Topics {
			plugins = append(plugins, &receiver.Topics[j].Config)
		}
	}
	for i := range out.Senders {
		plugins = append(plugins, &out.Senders[i].Config)
	}
	for i := range out.Bindings {
		plugins = append(plugins, &out.Bindings[i].Config)
	}
	for _, plugin := range plugins {
		*plugin, err = freezeInitialPlugin(*plugin)
		if err != nil {
			return nil, err
		}
	}
	for i := range out.Routes {
		route := &out.Routes[i]
		route.Bindings, route.Processors = slices.Clone(route.Bindings), slices.Clone(route.Processors)
		if route.Policy.Backoff.Jitter != nil {
			jitter := *route.Policy.Backoff.Jitter
			route.Policy.Backoff.Jitter = &jitter
		}
		if route.Resolver != nil {
			resolver := *route.Resolver
			resolver.HeaderMap = maps.Clone(resolver.HeaderMap)
			resolver.Rules = slices.Clone(resolver.Rules)
			for j := range resolver.Rules {
				resolver.Rules[j].Match = slices.Clone(resolver.Rules[j].Match)
				for k := range resolver.Rules[j].Match {
					value := &resolver.Rules[j].Match[k].Value
					*value, err = cloneInitialValue(*value)
					if err != nil {
						return nil, err
					}
				}
			}
			route.Resolver = &resolver
		}
		if route.Session != nil {
			session := *route.Session
			if session.DrainStrategy != nil {
				drain := *session.DrainStrategy
				session.DrainStrategy = &drain
			}
			if session.ConnectAfterLease != nil {
				connect := *session.ConnectAfterLease
				session.ConnectAfterLease = &connect
			}
			route.Session = &session
		}
	}
	return out, nil
}

func freezeInitialPlugin(cfg ports.PluginConfig) (ports.PluginConfig, error) {
	if cfg == nil {
		return nil, nil
	}
	if ports.IsNilPluginConfig(cfg) {
		return nil, shared.ErrInvalidConfig.WithMessage("config initialization: typed-nil plugin")
	}
	freezer, ok := cfg.(ports.FreezableConfig)
	if !ok {
		if immutableInitialType(reflect.TypeOf(cfg)) {
			return cfg, nil // scalar value configs have no mutable aliases.
		}
		return nil, shared.ErrNotSupported.WithMessage("config initialization: mutable plugin requires FreezableConfig")
	}
	frozen := freezer.FreezePluginConfig()
	if ports.IsNilPluginConfig(frozen) || frozen.Kind() != cfg.Kind() {
		return nil, shared.ErrInvalidConfig.WithMessage("config initialization: invalid frozen plugin")
	}
	for _, capability := range []reflect.Type{
		reflect.TypeFor[ports.FreezableConfig](),
		reflect.TypeFor[ports.CredentialedConfig](),
		reflect.TypeFor[ports.DurableSessionIdentityConfig](),
		reflect.TypeFor[ports.PostAcquireActivationTimingConfig](),
		reflect.TypeFor[ports.TransportFailoverTimingConfig](),
		reflect.TypeFor[ports.IngressMemoryConfig](),
	} {
		if reflect.TypeOf(cfg).Implements(capability) && !reflect.TypeOf(frozen).Implements(capability) {
			return nil, shared.ErrInvalidConfig.WithMessage("config initialization: frozen plugin lost a capability")
		}
	}
	return frozen, nil
}

// Inspect types, never reflect-clone adapter-owned values or opaque handles.
func immutableInitialType(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Bool, reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return true
	case reflect.Array:
		return immutableInitialType(t.Elem())
	case reflect.Struct:
		for i := range t.NumField() {
			if !immutableInitialType(t.Field(i).Type) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func cloneInitialValue(value any) (any, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			cloned, err := cloneInitialValue(item)
			if err != nil {
				return nil, err
			}
			out[key] = cloned
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			cloned, err := cloneInitialValue(item)
			if err != nil {
				return nil, err
			}
			out[i] = cloned
		}
		return out, nil
	case map[string]string:
		return maps.Clone(v), nil
	case []string:
		return slices.Clone(v), nil
	case []byte:
		return slices.Clone(v), nil
	default:
		if immutableInitialType(reflect.TypeOf(value)) {
			return value, nil
		}
		return nil, fmt.Errorf("config initialization: unsupported mutable condition value %T: %w", value, shared.ErrInvalidConfig)
	}
}
