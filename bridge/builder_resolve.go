package bridge

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/domain/shared"
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

// cloneConfigForBuild copies the bridge-owned blueprint graph and asks each
// adapter to freeze its own opaque PluginConfig when it advertises
// ports.FreezableConfig. Unknown plugin configs remain intentionally opaque and
// shared: bridge never copies mutexes, clients, map keys, or private state it
// cannot understand. Configs that the bridge mutates or uses for durable identity
// must be adapter-freezable so rollback and preflight state cannot be corrupted.
func cloneConfigForBuild(cfg *ports.BridgeConfig) (*ports.BridgeConfig, error) {
	if cfg == nil {
		return nil, nil
	}
	out := *cfg
	if cfg.ConfigWatch != nil {
		copy := *cfg.ConfigWatch
		out.ConfigWatch = &copy
	}
	if cfg.Bridge.Cluster != nil {
		cluster := *cfg.Bridge.Cluster
		cluster.Endpoints = cloneStringMap(cfg.Bridge.Cluster.Endpoints)
		out.Bridge.Cluster = &cluster
	}
	var err error
	if out.Stores.Lease, err = cloneStoreConfig(cfg.Stores.Lease); err != nil {
		return nil, fmt.Errorf("bridge: freeze lease store config: %w", err)
	}
	if out.Stores.Outbox, err = cloneStoreConfig(cfg.Stores.Outbox); err != nil {
		return nil, fmt.Errorf("bridge: freeze outbox store config: %w", err)
	}
	if out.Stores.DLQ, err = cloneStoreConfig(cfg.Stores.DLQ); err != nil {
		return nil, fmt.Errorf("bridge: freeze dlq store config: %w", err)
	}
	if out.Stores.ManagedSubscriptions, err = cloneStoreConfig(cfg.Stores.ManagedSubscriptions); err != nil {
		return nil, fmt.Errorf("bridge: freeze managed subscription store config: %w", err)
	}

	out.Sessions = append([]ports.SessionDef(nil), cfg.Sessions...)
	for i := range out.Sessions {
		out.Sessions[i].Config, err = freezePluginConfig(out.Sessions[i].Config)
		if err != nil {
			return nil, fmt.Errorf("bridge: freeze session %q config: %w", out.Sessions[i].ID, err)
		}
	}
	out.Receivers = append([]ports.ReceiverDef(nil), cfg.Receivers...)
	for i := range out.Receivers {
		out.Receivers[i].Config, err = freezePluginConfig(out.Receivers[i].Config)
		if err != nil {
			return nil, fmt.Errorf("bridge: freeze receiver %q config: %w", out.Receivers[i].ID, err)
		}
		out.Receivers[i].Topics = append([]ports.SubscriptionDef(nil), cfg.Receivers[i].Topics...)
		for j := range out.Receivers[i].Topics {
			out.Receivers[i].Topics[j].Config, err = freezePluginConfig(out.Receivers[i].Topics[j].Config)
			if err != nil {
				return nil, fmt.Errorf("bridge: freeze receiver %q topic config: %w", out.Receivers[i].ID, err)
			}
		}
	}
	out.Senders = append([]ports.SenderDef(nil), cfg.Senders...)
	for i := range out.Senders {
		out.Senders[i].Config, err = freezePluginConfig(out.Senders[i].Config)
		if err != nil {
			return nil, fmt.Errorf("bridge: freeze sender %q config: %w", out.Senders[i].ID, err)
		}
	}
	out.Bindings = append([]ports.BindingDef(nil), cfg.Bindings...)
	for i := range out.Bindings {
		out.Bindings[i].Config, err = freezePluginConfig(out.Bindings[i].Config)
		if err != nil {
			return nil, fmt.Errorf("bridge: freeze binding %q config: %w", out.Bindings[i].ID, err)
		}
	}
	out.Routes = append([]ports.RouteDef(nil), cfg.Routes...)
	for i := range out.Routes {
		cloneRouteDef(&out.Routes[i], &cfg.Routes[i])
	}
	if cfg.HTTP != nil {
		copy := *cfg.HTTP
		out.HTTP = &copy
	}
	return &out, nil
}

func freezePluginConfig(config ports.PluginConfig) (ports.PluginConfig, error) {
	if config == nil {
		return nil, nil
	}
	if ports.IsNilPluginConfig(config) {
		return nil, shared.ErrInvalidConfig.WithMessage("bridge: typed-nil plugin config")
	}

	sourceKind := config.Kind()
	freezable, canFreeze := config.(ports.FreezableConfig)
	_, credentialed := config.(ports.CredentialedConfig)
	_, durable := config.(ports.DurableSessionIdentityConfig)
	_, activationTimed := config.(ports.PostAcquireActivationTimingConfig)
	_, ingressMemoryAware := config.(ports.IngressMemoryConfig)
	if !canFreeze {
		if credentialed {
			return nil, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("bridge: credentialed plugin config kind %q does not support freezing", sourceKind))
		}
		if durable {
			return nil, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("bridge: durable plugin config kind %q does not support freezing", sourceKind))
		}
		return config, nil
	}

	frozen := freezable.FreezePluginConfig()
	if frozen == nil || ports.IsNilPluginConfig(frozen) {
		return nil, shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("bridge: plugin config kind %q froze to nil", sourceKind))
	}
	if frozen.Kind() != sourceKind {
		return nil, shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("bridge: plugin config kind %q froze to a different kind", sourceKind))
	}
	if durable {
		if _, ok := frozen.(ports.DurableSessionIdentityConfig); !ok {
			return nil, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("bridge: durable plugin config kind %q lost its identity capability when frozen", sourceKind))
		}
	}
	if activationTimed {
		if _, ok := frozen.(ports.PostAcquireActivationTimingConfig); !ok {
			return nil, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("bridge: plugin config kind %q lost its post-acquire activation timing capability when frozen", sourceKind))
		}
		if ingressMemoryAware {
			if _, ok := frozen.(ports.IngressMemoryConfig); !ok {
				return nil, shared.ErrInvalidConfig.WithMessage(
					fmt.Sprintf("bridge: plugin config kind %q lost its ingress memory capability when frozen", sourceKind))
			}
		}
	}
	if credentialed {
		if _, ok := frozen.(ports.CredentialedConfig); !ok {
			return nil, shared.ErrInvalidConfig.WithMessage(
				fmt.Sprintf("bridge: credentialed plugin config kind %q lost its credential capability when frozen", sourceKind))
		}
	}
	if _, ok := frozen.(ports.FreezableConfig); !ok {
		return nil, shared.ErrInvalidConfig.WithMessage(
			fmt.Sprintf("bridge: plugin config kind %q lost its freeze capability when frozen", sourceKind))
	}
	return frozen, nil
}

func cloneStoreConfig(config *ports.StoreConfig) (*ports.StoreConfig, error) {
	if config == nil {
		return nil, nil
	}
	copy := *config
	var err error
	copy.Config, err = freezePluginConfig(config.Config)
	if err != nil {
		return nil, err
	}
	return &copy, nil
}

func cloneRouteDef(dst, src *ports.RouteDef) {
	dst.Bindings = append([]string(nil), src.Bindings...)
	dst.Processors = append([]string(nil), src.Processors...)
	if src.Resolver != nil {
		resolver := *src.Resolver
		resolver.HeaderMap = cloneStringMap(src.Resolver.HeaderMap)
		resolver.Rules = append([]ports.RuleDef(nil), src.Resolver.Rules...)
		for i := range resolver.Rules {
			resolver.Rules[i].Match = append([]ports.ConditionDef(nil), src.Resolver.Rules[i].Match...)
			for j := range resolver.Rules[i].Match {
				resolver.Rules[i].Match[j].Value = cloneBlueprintValue(resolver.Rules[i].Match[j].Value)
			}
		}
		dst.Resolver = &resolver
	}
	if src.Session != nil {
		session := *src.Session
		if src.Session.DrainStrategy != nil {
			strategy := *src.Session.DrainStrategy
			session.DrainStrategy = &strategy
		}
		if src.Session.ConnectAfterLease != nil {
			connectAfterLease := *src.Session.ConnectAfterLease
			session.ConnectAfterLease = &connectAfterLease
		}
		dst.Session = &session
	}
}

func cloneStringMap(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	out := make(map[string]string, len(src))
	for key, value := range src {
		out[key] = value
	}
	return out
}

func cloneBlueprintValue(value any) any {
	switch typed := value.(type) {
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = cloneBlueprintValue(typed[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = cloneBlueprintValue(item)
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	case []byte:
		return append([]byte(nil), typed...)
	case map[string]string:
		return cloneStringMap(typed)
	default:
		return value
	}
}
