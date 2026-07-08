package config

import (
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// DefaultMerge merges an overlay ports.BridgeConfig on top of a base config.
// The merge follows these rules:
//   - Version: overlay wins when non-zero, otherwise base is retained. Version
//     is an optimistic-concurrency counter, so a merged config carries the
//     newest committed version; a zero overlay means "unversioned overlay,
//     keep the base's version" rather than resetting to 0.
//   - Bridge settings: overlay non-zero fields override base
//   - Bridge.Cluster: overlay replaces base if non-nil (endpoint map cloned)
//   - ConfigWatch: overlay replaces base if non-nil
//   - Stores: overlay replaces base per store role (lease/outbox/dlq individually)
//   - Sessions, Receivers, Senders, Bindings: overlay adds new entries; for an
//     entry that matches an existing one by ID the overlay is merged FIELD-LEVEL
//     on top of the base entry — non-empty overlay scalar fields win and the
//     base entry's typed plugin Config (broker URLs, credentials) is CARRIED
//     FORWARD unless the overlay changed the transport discriminator. This keeps
//     a partial PATCH (e.g. only session_mode) from erasing the plugin options,
//     which the on-disk wire format would otherwise drop (json:"-") and destroy
//     the persisted config.
//   - Routes: overlay adds new entries or wholesale-replaces an existing entry
//     matching by ID (routes carry no plugin Config, so no options can be lost)
//   - HTTP: overlay is merged field-level on top of base; non-empty overlay
//     scalar fields win, and the API-key secrets are preserved when the overlay
//     omits them or echoes back the redaction marker
//
// The base is not modified; a new ports.BridgeConfig is returned.
func DefaultMerge(base, overlay *ports.BridgeConfig) (*ports.BridgeConfig, error) {
	out := *base

	// Version is an optimistic-concurrency counter: a non-zero overlay version
	// is the newer committed version and wins; a zero overlay leaves the base
	// version intact instead of silently dropping it to 0 (Finding 8).
	if overlay.Version != 0 {
		out.Version = overlay.Version
	}

	mergeBridgeSettings(&out.Bridge, &overlay.Bridge)

	// Defensive deep-clone: when the overlay carries no cluster block,
	// out.Bridge.Cluster is still the base's pointer (aliased via `out := *base`
	// above, since mergeBridgeSettings only reassigns it when the overlay set
	// one). Clone it so a later mutation of the merged config cannot reach back
	// into the base. No observable behavior change.
	if overlay.Bridge.Cluster == nil && out.Bridge.Cluster != nil {
		c := *out.Bridge.Cluster
		if out.Bridge.Cluster.Endpoints != nil {
			c.Endpoints = make(map[string]string, len(out.Bridge.Cluster.Endpoints))
			for k, v := range out.Bridge.Cluster.Endpoints {
				c.Endpoints[k] = v
			}
		}
		out.Bridge.Cluster = &c
	}

	if overlay.ConfigWatch != nil {
		cw := *overlay.ConfigWatch
		out.ConfigWatch = &cw
	} else if out.ConfigWatch != nil {
		// Defensive deep-clone for symmetry with the HTTP/Cluster clones: with
		// no overlay ConfigWatch, out.ConfigWatch still aliases base.ConfigWatch
		// (via `out := *base`) so a later mutation of the merged config could
		// reach back into the cached base layer (Finding 10).
		cw := *out.ConfigWatch
		out.ConfigWatch = &cw
	}

	mergeStores(&out.Stores, &overlay.Stores)

	out.Sessions = mergeByID(base.Sessions, overlay.Sessions,
		func(s ports.SessionDef) string { return s.ID }, mergeSessionDef)
	out.Receivers = mergeByID(base.Receivers, overlay.Receivers,
		func(r ports.ReceiverDef) string { return r.ID }, mergeReceiverDef)
	out.Senders = mergeByID(base.Senders, overlay.Senders,
		func(s ports.SenderDef) string { return s.ID }, mergeSenderDef)
	out.Bindings = mergeByID(base.Bindings, overlay.Bindings,
		func(b ports.BindingDef) string { return b.ID }, mergeBindingDef)
	out.Routes = mergeByID(base.Routes, overlay.Routes,
		func(r ports.RouteDef) string { return r.ID }, overlayWins[ports.RouteDef])

	if overlay.HTTP != nil {
		merged := mergeHTTP(base.HTTP, overlay.HTTP)
		out.HTTP = &merged
	} else if out.HTTP != nil {
		// Defensive deep-clone: with no overlay HTTP block, out.HTTP still
		// aliases base.HTTP (via `out := *base`). Clone it for symmetry with
		// the Cluster clone above so a later mutation of the merged config
		// cannot reach back into the cached base layer (Finding 10).
		h := *out.HTTP
		out.HTTP = &h
	}

	return &out, nil
}

// mergeHTTP merges an overlay HTTP block field-level on top of base. A
// wholesale replace would clobber unspecified fields — in particular
// PATCHing only admin_addr would zero BOTH API keys, persist empty keys, and
// lock the operator out of the API needed to fix it. Non-empty overlay scalar
// fields win; empty overlay fields keep the base value; the two secrets are
// preserved when the overlay omits them (empty) or echoes back the redaction
// marker read from the redacted admin GET.
func mergeHTTP(base, overlay *ports.HTTPConfig) ports.HTTPConfig {
	out := ports.HTTPConfig{}
	if base != nil {
		out = *base
	}
	if overlay.AdminAddr != "" {
		out.AdminAddr = overlay.AdminAddr
	}
	if overlay.MonitorAddr != "" {
		out.MonitorAddr = overlay.MonitorAddr
	}
	if overlay.CORSOrigins != "" {
		out.CORSOrigins = overlay.CORSOrigins
	}
	if overlay.TLSCertFile != "" {
		out.TLSCertFile = overlay.TLSCertFile
	}
	if overlay.TLSKeyFile != "" {
		out.TLSKeyFile = overlay.TLSKeyFile
	}
	out.AdminAPIKey = preserveKeptSecret(out.AdminAPIKey, overlay.AdminAPIKey)
	out.MonitorAPIKey = preserveKeptSecret(out.MonitorAPIKey, overlay.MonitorAPIKey)
	return out
}

// preserveKeptSecret returns base when the overlay carries the redaction marker
// (a value echoed back from a redacted read) or is empty (the PATCH omitted the
// field). Only a genuinely new, non-empty, non-redacted secret overwrites the
// stored one. This stops a sanitized-config round-trip from persisting
// "[REDACTED]" and stops a partial PATCH from wiping a configured key.
func preserveKeptSecret(base, overlay shared.Secret) shared.Secret {
	if overlay.IsRedacted() || overlay.IsZero() {
		return base
	}
	return overlay
}

func mergeBridgeSettings(base, overlay *ports.BridgeSettings) {
	if overlay.ID != "" {
		base.ID = overlay.ID
	}
	if overlay.InstanceID != "" {
		base.InstanceID = overlay.InstanceID
	}
	if overlay.DeploymentMode != "" {
		base.DeploymentMode = overlay.DeploymentMode
	}
	if overlay.ShutdownTimeout != "" {
		base.ShutdownTimeout = overlay.ShutdownTimeout
	}
	if overlay.DrainTimeout != "" {
		base.DrainTimeout = overlay.DrainTimeout
	}
	if overlay.PerRecordDrainTimeout != "" {
		base.PerRecordDrainTimeout = overlay.PerRecordDrainTimeout
	}
	if overlay.MaxDrainTimeout != "" {
		base.MaxDrainTimeout = overlay.MaxDrainTimeout
	}
	if overlay.LogLevel != "" {
		base.LogLevel = overlay.LogLevel
	}
	// Cluster was silently dropped: an overlay that added or changed cluster
	// endpoints never took effect after a merge (Finding 8). Overlay replaces
	// base when set; the endpoint map is cloned so the merged config never
	// aliases the overlay's map.
	if overlay.Cluster != nil {
		c := *overlay.Cluster
		if overlay.Cluster.Endpoints != nil {
			c.Endpoints = make(map[string]string, len(overlay.Cluster.Endpoints))
			for k, v := range overlay.Cluster.Endpoints {
				c.Endpoints[k] = v
			}
		}
		base.Cluster = &c
	}
}

func mergeStores(base, overlay *ports.StoresConfig) {
	base.Lease = mergeStoreRole(base.Lease, overlay.Lease)
	base.Outbox = mergeStoreRole(base.Outbox, overlay.Outbox)
	base.DLQ = mergeStoreRole(base.DLQ, overlay.DLQ)
}

// mergeStoreRole returns a clone of the overlay store when set, otherwise a
// clone of the base store. Cloning the base (rather than aliasing its pointer
// via `out := *base`) keeps the merged config from reaching back into the
// cached base layer if a consumer later mutates the store entry (Finding 10 —
// symmetry with the overlay clone and the Cluster clone).
func mergeStoreRole(base, overlay *ports.StoreConfig) *ports.StoreConfig {
	if overlay != nil {
		sc := *overlay
		return &sc
	}
	if base != nil {
		sc := *base
		return &sc
	}
	return nil
}

// mergeByID merges overlay entries onto base entries keyed by id. A new overlay
// entry (no id match in base) is appended; an overlay entry whose id matches a
// base entry is combined via combine(baseEntry, overlayEntry). Order is
// preserved: existing entries keep their position, new ones append in overlay
// order.
func mergeByID[T any](base, overlay []T, id func(T) string, combine func(base, overlay T) T) []T {
	if len(overlay) == 0 {
		out := make([]T, len(base))
		copy(out, base)
		return out
	}

	index := make(map[string]int, len(base))
	out := make([]T, len(base))
	copy(out, base)
	for i, item := range out {
		index[id(item)] = i
	}

	for _, item := range overlay {
		if idx, ok := index[id(item)]; ok {
			out[idx] = combine(out[idx], item)
		} else {
			index[id(item)] = len(out)
			out = append(out, item)
		}
	}
	return out
}

// overlayWins is the collision strategy for entries that carry no plugin Config
// (routes): the overlay entry wholesale-replaces the base entry.
func overlayWins[T any](_, overlay T) T { return overlay }

// carriedPluginConfig decides which typed plugin Config (and its raw payload) a
// merged def keeps. The overlay wins when it carries its own decoded Config;
// otherwise the base Config is PRESERVED so a scalar-only PATCH never erases the
// plugin options — unless the overlay changed the transport/kind discriminator
// (overlayKind non-empty and different), in which case the stale base Config no
// longer matches the new kind and is dropped (nil). A nil Config on a changed
// kind is caught by the commit-time guard rather than silently persisted.
func carriedPluginConfig(
	baseCfg, overlayCfg ports.PluginConfig,
	baseRaw, overlayRaw ports.RawConfig,
	baseKind, overlayKind string,
) (ports.PluginConfig, ports.RawConfig) {
	switch {
	case overlayCfg != nil:
		return overlayCfg, overlayRaw
	case overlayKind != "" && overlayKind != baseKind:
		return nil, nil
	default:
		return baseCfg, baseRaw
	}
}

func mergeSessionDef(base, overlay ports.SessionDef) ports.SessionDef {
	out := base
	if overlay.Transport != "" {
		out.Transport = overlay.Transport
	}
	if overlay.SessionMode != "" {
		out.SessionMode = overlay.SessionMode
	}
	cfg, raw := carriedPluginConfig(base.Config, overlay.Config, base.Raw(), overlay.Raw(), base.Transport, overlay.Transport)
	out.SetDecoded(cfg, raw)
	return out
}

func mergeReceiverDef(base, overlay ports.ReceiverDef) ports.ReceiverDef {
	out := base
	if overlay.Transport != "" {
		out.Transport = overlay.Transport
	}
	if overlay.SessionID != "" {
		out.SessionID = overlay.SessionID
	}
	if len(overlay.Topics) > 0 {
		out.Topics = overlay.Topics
	}
	cfg, raw := carriedPluginConfig(base.Config, overlay.Config, base.Raw(), overlay.Raw(), base.Transport, overlay.Transport)
	out.SetDecoded(cfg, raw)
	return out
}

func mergeSenderDef(base, overlay ports.SenderDef) ports.SenderDef {
	out := base
	if overlay.Transport != "" {
		out.Transport = overlay.Transport
	}
	if overlay.SessionID != "" {
		out.SessionID = overlay.SessionID
	}
	cfg, raw := carriedPluginConfig(base.Config, overlay.Config, base.Raw(), overlay.Raw(), base.Transport, overlay.Transport)
	out.SetDecoded(cfg, raw)
	return out
}

func mergeBindingDef(base, overlay ports.BindingDef) ports.BindingDef {
	out := base
	if overlay.SenderID != "" {
		out.SenderID = overlay.SenderID
	}
	if overlay.SessionID != "" {
		out.SessionID = overlay.SessionID
	}
	if overlay.Address != "" {
		out.Address = overlay.Address
	}
	// A binding inherits its plugin kind from the referenced sender, so a
	// change of SenderID is the discriminator change that invalidates the
	// carried Config.
	cfg, raw := carriedPluginConfig(base.Config, overlay.Config, base.Raw(), overlay.Raw(), base.SenderID, overlay.SenderID)
	out.SetDecoded(cfg, raw)
	return out
}
