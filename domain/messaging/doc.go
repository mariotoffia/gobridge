// Package messaging defines the messaging-context domain types of the
// GoBridge bridge: the canonical Envelope value object, the reserved
// header vocabulary, and the W3C Trace Context parse/format/extract/
// inject helpers.
//
// This package is part of the innermost (Layer 1) ring of the
// architecture. It carries no project dependencies and no external
// dependencies — imports must remain stdlib-only.
package messaging
