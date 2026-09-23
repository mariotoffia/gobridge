---
applyTo: "adapters/native/store/**,adapters/native/memorylease/**,adapters/native/memoryrollout/**,adapters/native/config/**,adapters/aws/store/**,adapters/aws/config/**,ports/storetest/**,ports/configstoretest/**"
---

# Stores: outbox, DLQ, lease, rollout, managed subscriptions, config

Adds to `adapters.instructions.md`. Sources: ADR-0005, ADR-0007, ADR-0015,
`docs/internals/architecture-stores-and-configuration.md`,
`docs/internals/plugin-store-adapters.md` and
`docs/cluster/spec/cluster-config-rollout-protocol.md`.

## Conformance

- Every store runs its `ports/storetest` suite (`RunOutboxStoreTests`,
  `RunDLQStoreTests`, `RunLeaseStoreTests`, `RunClusterRolloutStoreTests`);
  config stores run `configstoretest.Run`. A missing check is added to the
  suite so every backend gets it — not written as a one-off in one adapter
  (TESTS.md §3.3).
- A change to a store port contract comes with a suite change in the same PR.

## Outbox (ADR-0005)

- `Claim` returns the oldest N by `(CreatedAt, Seq)`. `limit <= 0` is a
  fencing no-op. Selection never filters by replay count, and `QueryPending`
  is a preview, never used to select.
- Ordering-key head-of-line: a record is never claimed while an older sibling
  with the same key is non-terminal and not in the same batch.
- A short batch is legal: return `(claimed, nil)` and never drop records that
  were durably claimed. Only `shared.ErrStaleFencingToken` returns no records.
- DynamoDB: page until `LastEvaluatedKey == nil`; one fence row per partition
  holds `max_claim_version` and `seq_counter`; `Claim` condition-checks that row
  inside `TransactWriteItems`.

## Fencing and leases

- Every mutation that takes a `LeaseToken` rejects a stale token atomically,
  in the same conditional write. `Expire` is fenced like `Claim`.
- Lease writes are conditional and `LeaseToken.Version` only increases.

## DLQ

- `Write` returning nil means the entry survives a crash.
- The entry ID is derived from envelope ID, route, binding and source, never
  generated per write, so a repeated write collapses into one entry.
- `DLQReader` has no delete or purge; that is `DLQAdmin`. Redrive is not a
  store method — it is inject-then-delete in `httpapi` (ADR-0015).

## Rollout and config

- Rollout and lease tables are single-Region. Never a global table. Decision
  reads use `ConsistentRead`, never a GSI or a Stream. `Propose`, `Commit` and
  `Abort` are atomic compare-and-swap.
- Config `Save` is compare-and-swap on `BridgeConfig.Version`.
  `CreateIfAbsent` creates at version 1, never replaces a present document
  (even an invalid one), and re-reads the winner.
- Optional capabilities (`OutboxReleaser`, `OutboxDepthReporter`,
  `OutboxClaimedDepthReporter`) cost a feature when skipped, never a build
  break.

## Tests

- DynamoDB tests use `testutil/ddblocal` (DynamoDB Local), not the general AWS
  emulator: these stores are compare-and-swap end to end, and only DynamoDB
  Local is trusted to reject a failing `ConditionExpression` (TESTS.md §5).
