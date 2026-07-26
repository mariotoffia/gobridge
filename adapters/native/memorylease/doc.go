// Package memorylease implements ports.LeaseStore as an in-memory store.
//
// This adapter is suitable only for unit tests and single-process mode.
// It is not safe for clustered production deployments.
//
// Split-brain safety gate (finding c10-memlease-split): because the lease map
// and version counter are per-process, the store cannot detect — let alone
// coordinate — a deployment that has been scaled to more than one replica. To
// prevent silent split-brain ownership it FAILS CLOSED: every operation is
// rejected with shared.ErrInvalidConfig until the operator explicitly asserts
// single-replica ownership with WithAcknowledgeSingleReplica(true), which also
// emits one LOUD warning documenting the risk.
package memorylease
