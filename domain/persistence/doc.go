// Package persistence defines the persistence-context domain types of the
// GoBridge bridge: the durable Outbox state machine (OutboxRecord,
// OutboxStatus, OutboxPartitionKey), the cluster Lease fencing model
// (LeaseToken, LeaseInfo, PeerInfo), and the DrainStrategy polling
// policies that govern outbox drain cadence.
//
// This package is part of the innermost (Layer 1) ring of the
// architecture. Its only legitimate inbound dependencies are
// domain/shared and domain/messaging (because OutboxRecord carries an
// Envelope value). It carries no external dependencies — imports must
// remain stdlib-only.
package persistence
