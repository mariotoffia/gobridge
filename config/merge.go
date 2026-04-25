package config

// DefaultMerge merges an overlay BridgeConfig on top of a base config.
// The merge follows these rules:
//   - Bridge settings: overlay non-zero fields override base
//   - ConfigWatch: overlay replaces base if non-nil
//   - Stores: overlay replaces base per store role (lease/outbox/dlq individually)
//   - Sessions, Receivers, Senders, Bindings, Routes: overlay adds new entries or
//     replaces existing entries matching by ID
//   - HTTP: overlay replaces base if non-nil
//
// The base is not modified; a new BridgeConfig is returned.
func DefaultMerge(base, overlay *BridgeConfig) (*BridgeConfig, error) {
	out := *base

	mergeBridgeSettings(&out.Bridge, &overlay.Bridge)

	if overlay.ConfigWatch != nil {
		cw := *overlay.ConfigWatch
		out.ConfigWatch = &cw
	}

	mergeStores(&out.Stores, &overlay.Stores)

	out.Sessions = mergeByID(base.Sessions, overlay.Sessions, func(s SessionDef) string { return s.ID })
	out.Receivers = mergeByID(base.Receivers, overlay.Receivers, func(r ReceiverDef) string { return r.ID })
	out.Senders = mergeByID(base.Senders, overlay.Senders, func(s SenderDef) string { return s.ID })
	out.Bindings = mergeByID(base.Bindings, overlay.Bindings, func(b BindingDef) string { return b.ID })
	out.Routes = mergeByID(base.Routes, overlay.Routes, func(r RouteDef) string { return r.ID })

	if overlay.HTTP != nil {
		h := *overlay.HTTP
		out.HTTP = &h
	}

	return &out, nil
}

func mergeBridgeSettings(base, overlay *BridgeSettings) {
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
}

func mergeStores(base, overlay *StoresConfig) {
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
