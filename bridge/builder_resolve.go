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

// cloneConfigForBuild returns a copy of cfg whose per-attachment credentialed
// PluginConfig values (sessions, receivers, senders) are shallow-cloned so the
// in-place mutation performed by resolveConfigCredentials -> ApplyCredentials
// during a build does NOT pollute the caller's canonical config.
//
// Why this matters (Chunk 3, builder_resolve.go:42): ApplyCredentials mutates
// the config in place — it inlines the resolved secret material AND clears
// credentials_uri (see ports.CredentialedConfig). The Supervisor keeps the
// exact *ports.BridgeConfig it built from as its rollback/restart snapshot
// (s.cfg / oldCfg). Because a SessionDef.Config is an interface holding a
// pointer, mutating it reaches back into that retained snapshot: after a
// successful build the rollback config has credentials_uri erased and stale
// credentials inlined, so a later recoverOldOrWedge rebuild registers NO
// rotation watcher and starts with stale credentials -> auth failure after
// rollback. Cloning the credentialed configs before the build keeps the
// canonical config pristine and re-resolvable.
//
// The clone is intentionally SHALLOW per PluginConfig: reflect copies the whole
// pointed-to struct (including nested value structs and unexported secret
// fields), which covers every credential field ApplyCredentials writes today
// (top-level or nested value structs). Maps/slices inside a config remain
// shared with the original.
// ponytail: adapters whose ApplyCredentials mutates through a map/slice/pointer
// field (none do today) would need a deeper clone or a Clone() capability; the
// shallow copy is the smallest change that makes every current adapter's
// canonical config pristine.
func cloneConfigForBuild(cfg *ports.BridgeConfig) *ports.BridgeConfig {
	if cfg == nil {
		return nil
	}
	out := *cfg
	if n := len(cfg.Sessions); n > 0 {
		out.Sessions = make([]ports.SessionDef, n)
		copy(out.Sessions, cfg.Sessions)
		for i := range out.Sessions {
			out.Sessions[i].Config = clonePluginConfig(out.Sessions[i].Config)
		}
	}
	if n := len(cfg.Receivers); n > 0 {
		out.Receivers = make([]ports.ReceiverDef, n)
		copy(out.Receivers, cfg.Receivers)
		for i := range out.Receivers {
			out.Receivers[i].Config = clonePluginConfig(out.Receivers[i].Config)
		}
	}
	if n := len(cfg.Senders); n > 0 {
		out.Senders = make([]ports.SenderDef, n)
		copy(out.Senders, cfg.Senders)
		for i := range out.Senders {
			out.Senders[i].Config = clonePluginConfig(out.Senders[i].Config)
		}
	}
	return &out
}

// clonePluginConfig returns a shallow copy of a pointer-backed PluginConfig so
// ApplyCredentials can mutate the copy without touching the original. It copies
// the whole struct value (unexported fields included, which reflect field-level
// Set cannot do) via reflect.New + Set. Non-pointer or non-struct configs are
// returned unchanged: they are either already passed by value or carry no
// mutable identity worth cloning here.
func clonePluginConfig(pc ports.PluginConfig) ports.PluginConfig {
	if pc == nil {
		return nil
	}
	v := reflect.ValueOf(pc)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return pc
	}
	elem := v.Elem()
	if elem.Kind() != reflect.Struct {
		return pc
	}
	dup := reflect.New(elem.Type())
	dup.Elem().Set(elem)
	if cloned, ok := dup.Interface().(ports.PluginConfig); ok {
		return cloned
	}
	// The concrete type implemented PluginConfig on a value receiver rather
	// than a pointer receiver; the fresh pointer does not satisfy the
	// interface, so fall back to the original (it is not credential-mutated
	// through a pointer receiver anyway).
	return pc
}
