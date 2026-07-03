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
//   - Sessions, Receivers, Senders, Bindings, Routes: overlay adds new entries or
//     replaces existing entries matching by ID
//   - HTTP: overlay replaces base if non-nil
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

	if overlay.ConfigWatch != nil {
		cw := *overlay.ConfigWatch
		out.ConfigWatch = &cw
	}

	mergeStores(&out.Stores, &overlay.Stores)

	out.Sessions = mergeByID(base.Sessions, overlay.Sessions, func(s ports.SessionDef) string { return s.ID })
	out.Receivers = mergeByID(base.Receivers, overlay.Receivers, func(r ports.ReceiverDef) string { return r.ID })
	out.Senders = mergeByID(base.Senders, overlay.Senders, func(s ports.SenderDef) string { return s.ID })
	out.Bindings = mergeByID(base.Bindings, overlay.Bindings, func(b ports.BindingDef) string { return b.ID })
	out.Routes = mergeByID(base.Routes, overlay.Routes, func(r ports.RouteDef) string { return r.ID })

	if overlay.HTTP != nil {
		h := *overlay.HTTP
		var baseHTTP ports.HTTPConfig
		if base.HTTP != nil {
			baseHTTP = *base.HTTP
		}
		// A client that PATCHes an HTTP block previously read back from the
		// redacted admin GET carries "[REDACTED]" in the API-key fields. Treat
		// the redaction marker as "unchanged" so it never overwrites the real
		// stored secret; a genuinely new key still wins, and an empty overlay
		// key still clears (preserving the existing wholesale-replace semantics).
		h.AdminAPIKey = preserveRedactedSecret(baseHTTP.AdminAPIKey, h.AdminAPIKey)
		h.MonitorAPIKey = preserveRedactedSecret(baseHTTP.MonitorAPIKey, h.MonitorAPIKey)
		out.HTTP = &h
	}

	return &out, nil
}

// preserveRedactedSecret returns base when overlay carries the redaction
// marker (a value echoed back from a redacted read), otherwise overlay. This
// stops a sanitized-config round-trip from persisting "[REDACTED]" as a real
// secret on commit.
func preserveRedactedSecret(base, overlay shared.Secret) shared.Secret {
	if overlay.IsRedacted() {
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
	if overlay.Lease != nil {
		sc := *overlay.Lease
		base.Lease = &sc
	}
	if overlay.Outbox != nil {
		sc := *overlay.Outbox
		base.Outbox = &sc
	}
	if overlay.DLQ != nil {
		sc := *overlay.DLQ
		base.DLQ = &sc
	}
}

func mergeByID[T any](base, overlay []T, id func(T) string) []T {
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
			out[idx] = item
		} else {
			index[id(item)] = len(out)
			out = append(out, item)
		}
	}
	return out
}
