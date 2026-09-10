// Package dynamodbrollout is a DynamoDB-backed adapter for
// ports.ClusterRolloutStore: the durable coordination barrier for a
// cluster-wide config rollout (Proposed → Staging → Committed|Aborted).
//
// # Single row, optimistic concurrency
//
// A rollout store is a SINGLE-POINT coordinator — one active rollout at a time
// so the whole aggregate lives in ONE item under a fixed
// partition key. Every mutation is a read-modify-write: the store reads the row
// with a strongly ConsistentRead, rehydrates the domain aggregate
// (persistence.RehydrateRollout), applies the requested transition, and writes
// the result back with a conditional PutItem gated on an unchanged monotonic
// revision counter (rev). A losing writer re-reads and retries, so concurrent
// commit/abort settle on exactly one terminal decision and concurrent acks
// never lose an update — the domain state machine, not a hand-rolled parallel
// one, is the source of truth for every invariant.
//
// A fresh Propose on an empty store uses `attribute_not_exists(#pk)` so exactly
// one racer opens generation 1 (the split-brain guard); the rest observe the
// now-present row and get ErrAlreadyExists.
//
// # Single Region
//
// This adapter is correct only against a SINGLE DynamoDB Region. Its safety
// rests on read-your-writes: a strongly consistent read followed by a
// conditional write, both against the same partition. DynamoDB Global Tables
// replicate asynchronously with last-writer-wins and DO NOT preserve
// conditional-write semantics across Regions, so a multi-Region deployment can
// silently accept two conflicting terminal decisions. Never point this store at
// a Global Table replica. It also never uses a GSI or DynamoDB Streams — the
// coordinator reads the one row it owns, directly and consistently.
//
// # No TTL
//
// The rollout row is coordination state, not a cache entry; the store never
// writes a DynamoDB TTL attribute. Enabling table TTL would let a reaper delete
// the singleton row mid-rollout. Operators must keep TTL disabled on this table.
package dynamodbrollout
