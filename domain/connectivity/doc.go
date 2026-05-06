// Package connectivity defines the connectivity-context domain types of
// the GoBridge bridge: session lifecycle modes, the SessionPlan
// reconciliation shape (Subscriptions/Publishers), and the credential
// material (passwords, TLS certificates, composite credential sets)
// used to authenticate transport connections.
//
// This package is part of the innermost (Layer 1) ring of the
// architecture. It carries no external dependencies and depends only
// on domain/shared.
package connectivity
