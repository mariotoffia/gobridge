// Package session owns the runtime's session-lifecycle primitives:
// lease acquisition, renewal, three-phase step-down, reconnect
// reconciliation, and the lease-state event stream the parent runtime
// observes.
//
// The package is a leaf within the runtime layer: it depends only on
// inward layers (`domain/*`, `ports`) and the cross-cutting `logging`
// utility. The runtime package consumes [Manager] through the standard
// inward dependency rule (parent → leaf). It MUST NOT depend on its
// parent (`runtime`) nor on any adapter, bridge, or composition-root
// code; treat any new outward edge here as a smell that the leaf is
// absorbing orchestration concerns it should publish back to runtime
// instead.
package session
