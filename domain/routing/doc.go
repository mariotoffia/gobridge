// Package routing defines the routing-context domain types of the
// GoBridge bridge: route policies (RoutePolicy, BackoffPolicy and
// associated mode/action enums), destination bindings, dispatch plans,
// and the dead-letter queue records that describe permanently failed
// envelopes.
//
// This package is part of the innermost (Layer 1) ring of the
// architecture. Its only legitimate inbound dependencies are
// domain/shared and domain/messaging (because DLQEntry carries an
// Envelope value). It carries no external dependencies — imports must
// remain stdlib-only.
package routing
