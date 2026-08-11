package validation

import (
	"errors"
	"fmt"
)

// ErrInvalidBridgeID is returned when cfg.Bridge.ID does not match
// the bridge-name regex required by the Validation Matrix. The
// matrix names the field "bridge.name"; on the typed Go side it is
// BridgeSettings.ID. The Error message documents the mapping.
var ErrInvalidBridgeID = errors.New("bridge.id: invalid value")

// ErrEndpointURL is returned when an entry in
// bridge.cluster.endpoints fails to parse as a URL or lacks a
// non-empty scheme + host.
type ErrEndpointURL struct {
	Key    string
	Value  string
	Reason string
}

// Error implements error.
func (e *ErrEndpointURL) Error() string {
	return fmt.Sprintf(
		"bridge.cluster.endpoints[%q] = %q: malformed URL: %s",
		e.Key, e.Value, e.Reason,
	)
}

// ErrFilesystemProfile is returned when a feature requested by the
// logical config is incompatible with the filesystem_replicated
// topology. RouteID identifies the offending route; Reason is the
// matrix wording (verbatim) for the specific clash.
type ErrFilesystemProfile struct {
	RouteID string
	Reason  string
}

// Error implements error.
func (e *ErrFilesystemProfile) Error() string {
	return fmt.Sprintf("bootstrap: route %q %s", e.RouteID, e.Reason)
}

// ErrStorePathOutsideMount is returned when a store config carries a
// filesystem path that is not under the EFS mount root. Store is the
// dotted store identifier (stores.lease / stores.outbox / stores.dlq /
// stores.managed_subscriptions),
// Path is the offending value, Mount is the configured mount root.
type ErrStorePathOutsideMount struct {
	Store string
	Path  string
	Mount string
}

// Error implements error.
func (e *ErrStorePathOutsideMount) Error() string {
	return fmt.Sprintf(
		"%s: path %q is outside the EFS mount %q",
		e.Store, e.Path, e.Mount,
	)
}

// ErrWorkerWritesControlOnly is returned when a worker-role node
// references a store path inside the control-only convention
// (<mount>/control-only/...). Conservative: only flagged when the
// prefix matches exactly.
type ErrWorkerWritesControlOnly struct {
	Store string
	Path  string
}

// Error implements error.
func (e *ErrWorkerWritesControlOnly) Error() string {
	return fmt.Sprintf(
		"%s: worker node references control-only path %q (RW reserved for control nodes)",
		e.Store, e.Path,
	)
}

// ErrPlaintextSecret wraps the aggregated error returned by
// bridgecfg.ScanForPlaintextSecrets so callers can detect "Phase 1
// failed because of a plaintext secret" via errors.Is without
// inspecting strings. The wrapped error carries the per-field
// detail.
var ErrPlaintextSecret = errors.New("plaintext secret detected in config")
