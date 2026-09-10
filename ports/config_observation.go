package ports

import "context"

// ConfigInitializer is an optional strict create-only capability, independent
// of writer authorization. Existing documents, even invalid or versionless
// ones, are never replaced. Success stamps cfg.Version=1; a losing creation
// returns false without changing cfg. Errors may have an unknown commit outcome:
// reload the target, never overwrite or delete it to compensate.
// The caller owns cfg and must not mutate it during the call. The store retains
// no pointer to it; created is meaningful only when err is nil.
type ConfigInitializer interface {
	CreateIfAbsent(context.Context, *BridgeConfig) (created bool, err error)
}

// ConfigObservationKind distinguishes document absence from read failure.
type ConfigObservationKind string

const (
	ConfigPresent   ConfigObservationKind = "present"
	ConfigMissing   ConfigObservationKind = "missing"
	ConfigReadError ConfigObservationKind = "read_error"
)

// ConfigObservation carries a logical snapshot or an error. Sequence orders
// observations, not persisted versions. Present has non-nil Config and nil Err;
// Missing and ReadError have nil Config and a classified error. Snapshots must
// not be mutated while in use; they must never contain resolved credentials.
type ConfigObservation struct {
	Kind     ConfigObservationKind
	Config   *BridgeConfig
	Err      error
	Sequence uint64
}

// ConfigObserver emits an initial snapshot followed by ordered observations.
// Observed Missing→Present transitions must not be coalesced. Cancellation
// closes the channel; neither nil configs nor closure mean document absence.
// Watch and Observe need not be supported concurrently on the same instance.
type ConfigObserver interface {
	Observe(context.Context) (<-chan ConfigObservation, error)
}
