---
applyTo: "adapters/aws/transport/**"
---

# AWS SQS transport

Adds to `adapters.instructions.md`. Sources: ADR-0002, ADR-0008 and
`docs/transports/sqs.md`.

- Identity lift in `convertMessage`: the idempotency key comes from the
  `x-bridge.idempotency-key` attribute; dedup ID and ordering key come **only**
  from native `MessageDeduplicationId` / `MessageGroupId`. The
  `x-bridge.dedup-id` and `x-bridge.ordering-key` attributes are ignored. The
  lift is unconditional and does not read `trust_bridge_headers` (ADR-0008).
- FIFO `MessageDeduplicationId` hashes the logical subject, never the
  destination address.
- Egress attributes are dropped by rank when over the limit: bridge-to-bridge
  headers go first; `traceparent` / `tracestate` and `x-bridge.idempotency-key`
  go last. FIFO fields never use an attribute slot (`sqs.md` §Egress attribute
  priority).
- `SendMessageBatch` per-entry errors use the same code policy as a single
  send before falling back to `SenderFault`. KMS, throttling and service faults
  stay retriable; otherwise a transient outage becomes a terminal reject.
- A message that fails conversion is left in the queue (no `DeleteMessage`) so
  the native redrive policy keeps the payload. Only `poison_max_receives` may
  delete, and its startup guards (`>= 2` unless `poison_drop_without_dlq`,
  strictly greater than the native `maxReceiveCount`) stay.
- An init failure is returned from `Run` and does not close `Started()`. A
  readiness probe must never see a ready route whose receiver failed.
- Timeouts stay bounded: `init_timeout` (default 30s), each poll gets
  `WaitTimeSeconds + 10`, `wait_time_seconds` defaults to 20.
- Envelope `CreatedAt` comes from the broker `SentTimestamp`, so TTL measures
  the real age.
- Queue discovery by tags never picks "the first queue", and the discovered
  name never replaces the logical selector in config.
- Credential rotation is build-first: `ApplyCredentials` builds the new client,
  then swaps it under the init lock. A failed build keeps the old client live.
