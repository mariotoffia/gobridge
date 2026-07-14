package bridge

import (
	"context"
	"fmt"

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
// cannot understand. Durable identity preflight separately requires the
// adapter-owned freeze capability before invoking identity methods.
func cloneConfigForBuild(cfg *ports.BridgeConfig) *ports.BridgeConfig {
	if cfg == nil {
		return nil
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
	out.Stores.Lease = cloneStoreConfig(cfg.Stores.Lease)
	out.Stores.Outbox = cloneStoreConfig(cfg.Stores.Outbox)
	out.Stores.DLQ = cloneStoreConfig(cfg.Stores.DLQ)

	out.Sessions = append([]ports.SessionDef(nil), cfg.Sessions...)
	for i := range out.Sessions {
		out.Sessions[i].Config = freezePluginConfig(out.Sessions[i].Config)
	}
	out.Receivers = append([]ports.ReceiverDef(nil), cfg.Receivers...)
	for i := range out.Receivers {
		out.Receivers[i].Config = freezePluginConfig(out.Receivers[i].Config)
		out.Receivers[i].Topics = append([]ports.SubscriptionDef(nil), cfg.Receivers[i].Topics...)
		for j := range out.Receivers[i].Topics {
			out.Receivers[i].Topics[j].Config = freezePluginConfig(out.Receivers[i].Topics[j].Config)
		}
	}
	out.Senders = append([]ports.SenderDef(nil), cfg.Senders...)
	for i := range out.Senders {
		out.Senders[i].Config = freezePluginConfig(out.Senders[i].Config)
	}
	out.Bindings = append([]ports.BindingDef(nil), cfg.Bindings...)
	for i := range out.Bindings {
		out.Bindings[i].Config = freezePluginConfig(out.Bindings[i].Config)
	}
	out.Routes = append([]ports.RouteDef(nil), cfg.Routes...)
	for i := range out.Routes {
		cloneRouteDef(&out.Routes[i], &cfg.Routes[i])
	}
	if cfg.HTTP != nil {
		copy := *cfg.HTTP
		out.HTTP = &copy
	}
	return &out
}

func freezePluginConfig(config ports.PluginConfig) ports.PluginConfig {
	if ports.IsNilPluginConfig(config) {
		return config
	}
	freezable, ok := config.(ports.FreezableConfig)
	if !ok {
		return config
	}
	return freezable.FreezePluginConfig()
}

func cloneStoreConfig(config *ports.StoreConfig) *ports.StoreConfig {
	if config == nil {
		return nil
	}
	copy := *config
	copy.Config = freezePluginConfig(config.Config)
	return &copy
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
