# Routes, Runtime & Validation Reference

> Part of the [Configuration Model Reference](configuration-reference.md). This
> half covers routes, the `http` API block, the ID reference graph, delivery
> hooks, programmatic builder/lifecycle notes, and the validation-rules summary.

## `routes` -- Message Routes

Routes define the message flow from a receiver through processors to bindings.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `id` | string | **yes** | -- | Unique route identifier |
| `receiver_id` | string | **yes** | -- | Reference to a receiver |
| `delivery_mode` | string | no | `direct_hold` | `direct_hold` or `shared_outbox` |
| `dispatch_mode` | string | no | `single` | `single` (first binding) or `fan_out` (all bindings) |
| `trust_bridge_headers` | bool | no | false | Preserve bridge-to-bridge propagated headers (correlation/causation/idempotency/dedup/ordering/tenant/forwarded/trace) from inbound deliveries instead of stripping all `x-bridge.*` at ingress. **Enable only on receivers fed exclusively by a trusted upstream bridge** (see below) |
| `policy` | object | no | -- | Delivery and retry policy |
| `bindings` | string[] | **yes** | -- | References to binding IDs (at least one) |
| `processors` | string[] | no | -- | Ordered list of processor names |
| `resolver` | object | no | -- | Content-based binding resolver (see [Resolver](#routesresolver----content-based-resolver)) |
| `session` | object | no | -- | Route session management (for exclusive sessions) |

### Delivery modes

`direct_hold` settles the source only once the destination has accepted, so its
precondition is that the source **redelivers a message it was never told to
settle** -- that is what makes the crash window between the send and the settle
recoverable. Sources that provide it: SQS, Azure Service Bus in PeekLock, AMQP
0-9-1, AMQP 1.0, and MQTT on a route whose session survives the process and whose
subscriptions are QoS 1 or 2 (see
[capabilities](transport-configuration.md#transport-capabilities-matrix)). An HTTP ingress is
admitted on the other argument -- the caller is still holding the request, so
nothing has been settled and the retry is theirs. A route the runtime turns down
is named at config load with the precondition it failed.

An MQTT route that meets it needs no outbox, no lease and no outbox partition; it
holds the broker delivery instead of copying it into a store. Reaching for
`shared_outbox` there adds a second durable hop in series for the same crash
window, and moves the durable copy out of the broker into a store the operator
now has to run. The ingress session needs no `session` block and no binding
naming it either: the receiver's own binding to the session is what connects it
and reconciles its subscriptions, with no lease and no partition. What a durable
session still needs is `stores.managed_subscriptions` -- the exact record of the
filters it installed on the broker -- with its baseline seeded before the first
start (see [MQTT durable sessions](transports/mqtt-durable-sessions.md#managed-subscription-history)).

- **`direct_hold`** -- Source held open until egress completes. No inter-instance fencing; destinations must handle duplicates idempotently in clustered mode. When a `resolver` is configured, multiple bindings are allowed -- the resolver selects one per message. **Rejected at config load for a clustered exclusive route whose ingress is the HTTP transport:** a request forwarded to an owner that has just stepped down can be sent by the old owner while the new owner handles a retry (forwarded HTTP requests skip the ownership re-check, and `direct_hold` carries no fencing token at the sender boundary), a bounded duplicate-send window across failover. Use `shared_outbox` for that class. Non-clustered or non-HTTP-exclusive `direct_hold` routes are unaffected.
- **`shared_outbox`** -- Source acknowledged after persisting to outbox. Outbox drainer delivers asynchronously. Requires `stores.outbox`.

### Dispatch modes
- **`single`** -- Send to first matching binding (or resolver-selected binding).
- **`fan_out`** -- Send to all bindings.

### Trusting bridge-to-bridge headers (`trust_bridge_headers`)

By default every route strips all reserved `x-bridge.*` headers from an inbound
delivery at ingress, so an external producer can never inject bridge metadata
(spoofed `tenant-id`, forged routing) and the correlation ID is minted fresh.
This is the safe posture and is correct for any receiver that faces untrusted
producers.

Set `trust_bridge_headers: true` **only** when the receiver is fed EXCLUSIVELY
by a trusted upstream GoBridge instance. In that mode the BRIDGE-TO-BRIDGE
PROPAGATED headers (`correlation-id`, `causation-id`, `idempotency-key`,
`dedup-id`, `ordering-key`, `tenant-id`, `forwarded-from`/`forwarded-hop`, and
the W3C trace context) survive the hop so cross-bridge correlation, idempotency
and tracing are preserved. The INTERNAL-ONLY headers (`route-id`,
`route-override`, `source-id`, `content-type`) are ALWAYS stripped regardless of
this flag, so enabling it never lets an inbound header steer routing. Enabling
it on a receiver reachable by untrusted producers would let them spoof
`tenant-id` and the idempotency/dedup keys.

### `routes[].policy` -- Delivery Policy

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `max_in_flight` | int | no | 100 | Max concurrent messages in this route |
| `ack_after` | string | no | `target_accept` | `target_accept` or `outbox_persist` |
| `max_replay_attempts` | int | no | 5 | Max times a record may be claimed before it is eligible for poison (claims include deferrals/reclaims; poisoning also requires the wall-clock `replay_budget` to be spent) |
| `replay_budget` | duration | no | `15m` | Wall-clock ceiling, measured from a record's FIRST attempt, on how long the outbox drainer keeps redelivering it. It is the **age** half of the poison gate and applies **AND**-ed with `max_replay_attempts`: a record is poisoned to the DLQ only once **both** the attempt count and this budget are spent, so raising one alone changes nothing. Negative is rejected at config load; `0` takes the default. |
| `max_outbox_depth` | int | no | 10000 | Max pending outbox records before backpressure |
| `on_expired` | string | no | `dlq` | `drop` or `dlq` |
| `on_permanent_failure` | string | no | `dlq` | `drop` or `dlq` |
| `on_filtered` | string | no | `drop` | `drop` or `dlq` |
| `backoff` | object | no | -- | Retry backoff ladder for this route (see [Retry Backoff](#routespolicybackoff----retry-backoff)) |
| `send_timeout` | duration | no | `30s` | Timeout for individual send operations |
| `depth_cache_ttl` | duration | no | `1s` | How long outbox depth counts are cached |
| `allow_unfenced` | bool | no | false | Allow direct_hold with shared consumer sources (risk: no fencing) |
| `allow_retry_drop` | bool | no | false | Suppress error when source cannot retry and no DLQ is configured |

**Filtered messages (`on_filtered`):**

A *filtered* message is one a processor intentionally discards by returning
`shared.ErrMessageFiltered` -- for example a filter processor whose allow-list or
deny-list rejects it. This is a routine outcome, not a fault, and is counted in
the `MessagesFiltered` metric. Unlike `on_expired` and `on_permanent_failure`,
which default to `dlq`, `on_filtered` defaults to `drop`: a filtered message is
dropped-with-metric and reaches the DLQ only when `on_filtered: dlq` is set
explicitly **and** a DLQ store is configured. Keeping it separate from
`on_permanent_failure` stops a high-volume filter from flooding the DLQ through
the permanent-failure sink. The default changed in this release -- see the
[migration note](release-notes.md#routing).

### `routes[].policy.backoff` -- Retry Backoff

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `initial_interval` | duration | no | `1s` | First retry delay. Must not be negative |
| `max_interval` | duration | no | `30s` | Maximum retry delay. Must not be negative |
| `multiplier` | float | no | 2.0 | Exponential backoff multiplier. Must be **>= 1**: a value below 1 shrinks each delay, so retries accelerate instead of backing off. `1` is a fixed retry interval |
| `jitter` | float | no | `0.2` | Equal-jitter fraction in `[0,1]` applied to each computed delay: `delay = d*(1-jitter) + rand[0, d*jitter)`. De-correlates retries across replicas so a whole fleet does not re-attempt a failed target on the same tick. **Omitting the field takes the `0.2` default; an explicit `jitter: 0` opts out** and keeps the deterministic exponential delay |

Every field above is checked before a config change is written, so a bad retry
policy is refused at the point you submit it rather than failing later, at the
next apply or restart.

#### What counts as a retry attempt

Two separate things are counted, and only some events count towards either.

**The attempt number that drives the backoff ladder** is the source transport's
own redelivery count when the transport supplies one (SQS, Azure Service Bus,
AMQP 1.0), and the bridge's own per-message attempt count when it does not
(MQTT, AMQP 0-9-1, HTTP). Both climb on each redelivery, so the delay grows for
every source. A source with no redelivery counter is no longer stuck retrying at
`initial_interval` forever.

**The replay budget** -- the count `max_replay_attempts` compares against -- is
spent only by a failure of the MESSAGE itself: a refused send, a failing
processor, a destination that cannot be resolved. It is deliberately NOT spent
by:

| Event | Why it does not count |
|---|---|
| The outbox partition is at capacity, or its depth query failed | The message queued behind a slow drainer. It was never attempted, so it must not arrive at the cap already exhausted and be poisoned on its first real failure |
| The outbox write exceeded the store-operation deadline | The store was slow. The same message is written the moment it recovers |
| The DLQ store refused the record | The DLQ backend is unhealthy. The message is redelivered so the evidence can be written, but the message did not fail again |
| The bridge cancelled the delivery -- shutdown, a reconfiguration swap, a route restart, including a panic that happened while it was being torn down | The bridge stopped its own work. Nothing was learned about the message |

A cancelled delivery is left **unsettled**: it is never acknowledged, dropped or
written to the DLQ, so the source redelivers it after the restart. This holds
even under `on_permanent_failure: drop`, and it is the reason a rolling restart
does not discard in-flight messages.

### `routes[].session` -- Route Session Management

For routes targeting exclusive sessions. Manages lease acquisition and outbox draining.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `session_id` | string | **yes** | -- | Reference to an exclusive session |
| `sender_id` | string | **yes** | -- | Reference to a sender on that session |
| `lease_ttl` | duration | no | `360s` | Lease validity duration. Effective production values below `5s` are rejected uniformly before any lease-store backend is opened. |
| `renew_interval` | duration | no | derived | Lease renewal interval (default: lease_ttl / max_renew_fails) |
| `lease_renew_jitter` | duration | no | derived | Bounded random jitter added to each renewal timer to avoid a cluster-wide renewal thundering herd. Empty means the session manager derives it from `renew_interval`. |
| `max_renew_fails` | int | no | 3 | Consecutive renewal failures before step-down |
| `step_down_grace` | duration | no | `15s` | Grace period before releasing lease |
| `drain_interval` | duration | no | -- | Fixed outbox drain poll interval (mutually exclusive with drain_strategy) |
| `drain_batch_size` | int | no | 100 | Records per drain poll |
| `drain_max_batch_size` | int | no | 500 | Upper limit for adaptive batch scaling |
| `drain_max_concurrency` | int | no | 10 | Max concurrent send goroutines per drain cycle |
| `drain_strategy` | object | no | -- | Advanced drain polling strategy |
| `connect_after_lease` | bool | no | `true` | Defer the source transport connect until this instance wins the lease. Omitted resolves to `true` -- the safe default for the exclusive single-owner session a route source always is, since it stops a booting standby from resuming a broker-persisted subscription and consuming without the lease. Set `false` to opt out. |
| `renew_call_timeout` | duration | no | derived | Bounds a single lease-renew store call, so a hung backend cannot stretch step-down and takeover unboundedly. Folded into the failover-safety invariant below. Empty derives `min(renew_interval/2, 5s)` (floor 1s). |
| `acquire_poll_interval` | duration | no | derived | How often a standby retries acquiring the lease while another instance owns it. Empty derives `min(renew_interval, lease_ttl/4, 5s)`. Rejected below the `250ms` cadence floor. Declared SLO validation budgets two independent `max(1ms, ceil(1.25 × interval))` boundaries for positive jitter. |
| `broker_health_step_down` | duration or `off` | cond. | undeclared | How long an active owner may stay non-converged on its broker path (disconnected, or connected but not re-subscribed) before it releases the lease so a healthy standby takes over. `off` is the explicit decision not to fail over on a node-local broker outage. **Required when `failover_slo` is declared** -- omitted, the objective would silently exclude that failure mode. See [Failover budget](failover-budget.md#the-broker-path-decision). |
| `failover_slo` | duration | no | undeclared | Optional failure-detection-to-`ServiceLevelFull` objective, admitted against **both** failure modes. Must be positive when present. If timing capability or any budget term is unknown, startup fails closed. |
| `startup_allowance` | duration | no | `0s` | Explicit nonnegative process-start allowance added to a declared failover budget. Maximum `10m`. |

When `renew_interval` is set explicitly, cross-field validation requires
`(renew_interval + lease_renew_jitter/2 + renew_call_timeout) × max_renew_fails
< lease_ttl` so a renewal storm can never outlast the lease (a split-brain
guard). The per-call `renew_call_timeout` is part of the span because the renew
loop resets its timer only **after** each renew call returns, so a hung backend
that burns the full timeout on every attempt widens the real detection window.
When `renew_interval` is left empty the interval, jitter, and call timeout are
all derived and this cross-field check is skipped -- the derived values satisfy
it by construction.

**Resolved-cadence rules.** Blueprint validation additionally resolves the
cadence exactly as the session manager does -- through the same domain code, so
the rejection lands at commit rather than after the durable write -- defaults, derivation, then the expiry-margin
clamp -- and rejects an exclusive session on either of two grounds.

*The clamp had to cut the renew interval or the per-call timeout.* This is what
guards the **derived** path, which the cross-field check above cannot see because
that check only runs on a pinned `renew_interval`. A large `max_renew_fails`
against a modest `lease_ttl` leaves no per-attempt budget: `lease_ttl: 5s` with
`max_renew_fails: 5`, or `lease_ttl: 45s` with `max_renew_fails: 50`, both
collapse to a 1ms renew interval and a 1ms standby poll. The owner then renews
back to back while every standby issues a full claim round per millisecond; the
store throttles, and those throttling errors are counted as transient renew
failures -- a self-inflicted overload that ends in an ownership change. Lower
`max_renew_fails`, raise `lease_ttl`, or pin a shorter `renew_interval`. A clamp
that only sheds `lease_renew_jitter` is **not** rejected: jitter exists to spread
renewal load, the clamp trims it first by design, and what remains is a healthy
cadence (the session manager logs a warning).

*The resolved `renew_interval` or `acquire_poll_interval` is below `250ms`.* In
practice this binds on explicitly pinned values -- a derived cadence that is
below the floor has always been clamped first, so the rule above catches it. The
floor is the cadence below which the lease store, not the timing model, decides
ownership.

**Declared failover budget.** When `failover_slo` is present, preflight requires
that BOTH failure modes fit it -- owner death, anchored on `lease_ttl`, and a
node-local broker-path outage, anchored on `broker_health_step_down` -- using
checked duration arithmetic, before stores and transports are opened. The exact
boundary passes. Shared session IDs are first-wins at runtime, so preflight
canonicalizes every route/binding manager input per session and rejects any
divergence, which makes route order irrelevant.

A session that declares NO `failover_slo` still gets its computed budget logged
at every build, so the number is on record before an incident.

Both formulas, both shipped lease profiles evaluated at their defaults, and what
to measure before making a latency claim: [Failover budget](failover-budget.md).
`session.HAConfig` is a lease-renewal cadence, not an end-to-end preset.

### `routes[].session.drain_strategy` -- Drain Polling Strategy

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `type` | string | **yes** | -- | `fixed_poll` or `adaptive_backoff` |
| `interval` | duration | no | -- | Fixed poll interval (for `fixed_poll`) |
| `min_interval` | duration | no | -- | Minimum interval (for `adaptive_backoff`) |
| `max_interval` | duration | no | -- | Maximum interval (for `adaptive_backoff`) |
| `multiplier` | float | no | -- | Backoff multiplier (must be > 1.0) |

```yaml
routes:
  - id: durable-forward
    receiver_id: mqtt-in
    delivery_mode: shared_outbox
    dispatch_mode: single
    bindings: [to-sqs]
    processors: [my-filter, my-transform]
    policy:
      max_in_flight: 50
      ack_after: outbox_persist
      max_replay_attempts: 5
      on_permanent_failure: dlq
      backoff:
        initial_interval: 2s
        max_interval: 120s
        multiplier: 2.5
    session:
      session_id: mqtt-exclusive
      sender_id: sqs-out
      lease_ttl: 300s
      step_down_grace: 20s
      failover_slo: 980s
      startup_allowance: 10s
      drain_strategy:
        type: adaptive_backoff
        min_interval: 500ms
        max_interval: 10s
        multiplier: 1.5
```

### `routes[].resolver` -- Content-Based Resolver

Configures config-driven binding selection based on envelope content (headers, subject, JSON payload). When set, the resolver determines which binding receives each message. This replaces programmatic `MatchFunc` usage for most content-based routing scenarios.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `type` | string | **yes** | -- | Resolver type: `rules`, `header_map`, `all`, `static` |
| `default_binding` | string | no | -- | Fallback binding ID when no rule matches |
| `header_key` | string | cond. | -- | Header name to match (required for `header_map`) |
| `header_map` | map[string]string | cond. | -- | Header value to binding ID mapping (required for `header_map`) |
| `rules` | array | cond. | -- | Ordered rule list (required for `rules` type unless `default_binding` is set) |

**Resolver types:**
- **`rules`** -- Ordered first-match-wins rule evaluation. Each rule pairs conditions with a binding ID.
- **`header_map`** -- Maps a single header value directly to a binding ID. Simpler than `rules` for key-value routing.
- **`all`** -- Selects all bindings (fan-out).
- **`static`** -- Always selects the first binding.

#### `routes[].resolver.rules[]` -- Rule Definitions

Each rule is evaluated in order. The first rule whose conditions all match (AND logic) selects the binding.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `binding_id` | string | **yes** | -- | Target binding ID (must be in the route's `bindings` list) |
| `match` | array | no | -- | Conditions that must all be true. Empty `match` means the rule always matches (catch-all). |

#### `routes[].resolver.rules[].match[]` -- Condition Definitions

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `field` | string | **yes** | -- | Field to evaluate (see field patterns below) |
| `operator` | string | **yes** | -- | Comparison operator |
| `value` | any | cond. | -- | Value to compare against (not required for `exists`) |

**Field patterns:**

| Pattern | Description | Example |
|---------|-------------|---------|
| `subject` | Envelope subject | `subject` |
| `header.<key>` | Envelope header by key | `header.factory_id` |
| `$.<path>` | JSON payload path (dot-separated) | `$.metadata.region` |
| bare name | Header fallback (same as `header.<name>`) | `factory_id` |

**Operators:**

| Operator | Description | Value Type |
|----------|-------------|------------|
| `eq` | Equal (deep comparison) | any |
| `ne` | Not equal | any |
| `prefix` | String starts with | string |
| `contains` | String contains | string |
| `regex` | Regular expression match (max 4096 chars) | string |
| `gt` | Greater than (numeric) | number |
| `lt` | Less than (numeric) | number |
| `gte` | Greater than or equal | number |
| `lte` | Less than or equal | number |
| `exists` | Field exists (or not, when value is `false`) | bool |
| `in` | Value is in list | array |

**Example -- rules-based multi-tenant routing:**

```yaml
routes:
  - id: tenant-router
    receiver_id: http-in
    delivery_mode: direct_hold
    bindings: [bind-acme, bind-globex, bind-default]
    resolver:
      type: rules
      default_binding: bind-default
      rules:
        - binding_id: bind-acme
          match:
            - field: header.x-tenant
              operator: eq
              value: acme
        - binding_id: bind-globex
          match:
            - field: header.x-tenant
              operator: eq
              value: globex
```

**Example -- header_map shorthand:**

```yaml
routes:
  - id: tenant-router
    receiver_id: http-in
    bindings: [bind-acme, bind-globex]
    resolver:
      type: header_map
      header_key: x-tenant
      header_map:
        acme: bind-acme
        globex: bind-globex
```

**Example -- JSON payload routing:**

```yaml
routes:
  - id: priority-router
    receiver_id: sqs-in
    bindings: [bind-high, bind-normal]
    resolver:
      type: rules
      default_binding: bind-normal
      rules:
        - binding_id: bind-high
          match:
            - field: $.priority
              operator: eq
              value: high
```

When using a `rules` resolver with `direct_hold` delivery mode, each binding may reference a different sender. The runtime uses a **SenderRegistry** to dispatch messages to the correct sender based on the resolved binding. This enables true multi-sender routing from a single route definition.

## `http` -- HTTP API Configuration

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `admin_addr` | string | no | `:8080` | Admin server listen address |
| `monitor_addr` | string | no | `:8081` | Monitor server listen address |
| `admin_api_key` | string | **yes** | -- | API key (minimum 16 characters) |
| `monitor_api_key` | string | no | admin key | Separate monitor API key (min 16 chars when set) |
| `cors_origins` | string | no | disabled | CORS allowed origins (wildcard `*` rejected) |
| `tls_cert_file` | string | no | -- | Path to the PEM server certificate (with any intermediate chain). Enables in-process TLS on both listeners when paired with `tls_key_file`. |
| `tls_key_file` | string | no | -- | Path to the PEM private key for `tls_cert_file`. |

> **Restart required.** The admin and monitor servers are bound once, when the
> process starts, from the configuration it booted with. Changing
> `admin_addr`, `monitor_addr`, `cors_origins`, `tls_cert_file` or
> `tls_key_file` through a reload (file or admin config API) is accepted and
> stored durably, but the running listeners keep their original settings until
> the process is restarted. Adding an `http` block to a process that started
> without one likewise creates no servers. The API keys are the exception --
> a deployment that resolves them through a secret provider picks up a rotation
> on the next request. Where the composition root can see the divergence it
> reports it in the `restart_required` field of the `/deephealth`
> `config_watch` projection.

TLS is opt-in and both-or-none: when `tls_cert_file` and `tls_key_file` are both
set, the admin and monitor servers serve HTTPS with that pair; when either is
empty the servers stay plaintext (the historical default, assuming an external
LB/ingress/mesh terminates TLS). Supplying only one of the pair is a startup
error.

API keys are compared using SHA-256 constant-time hashing. Monitor endpoints
accept the monitor key or fall back to the admin key (admin is a superset).
Failed auth returns HTTP 401 with `WWW-Authenticate: Bearer` per RFC 9110.

See [Credentials & HTTP API](credentials-and-http-api.md) for endpoint documentation and
[OpenAPI specs](../spec/httpapi/http-api.yaml) for machine-readable definitions.

## ID Reference Graph

All IDs are validated for existence at config load time:

```mermaid
graph LR
    Route -->|receiver_id| Receiver
    Route -->|bindings[]| Binding
    Route -->|session.session_id| Session
    Route -->|session.sender_id| Sender
    Binding -->|sender_id| Sender
    Binding -.->|session_id| Session
    Receiver -.->|session_id| Session
    Sender -.->|session_id| Session

    style Route fill:#f96,stroke:#333
    style Session fill:#6bf,stroke:#333
```

Solid arrows are required references. Dashed arrows are optional. The validator rejects configs with broken references.

## Programmatic API

Delivery hooks, the programmatic builder and runtime lifecycle notes are in
[Programmatic API](programmatic-api.md).

## Validation Rules Summary

Errors from `config.Validate()`:

| Rule | Error Message |
|------|---------------|
| Missing bridge ID | `bridge.id is required` |
| Invalid deployment mode | `must be one of: standalone, clustered` |
| Duplicate IDs within section | `duplicate id "x"` |
| Broken reference | `session_id "x" not found in sessions` |
| shared_outbox without outbox store | `shared_outbox requires stores.outbox` |
| Exclusive session without lease store | `exclusive session requires stores.lease` |
| Clustered MQTT without `$share/` or exclusive | `requires $share/ topic prefix or exclusive session` |
| Invalid duration | `invalid duration "x"` |
| Mutually exclusive drain config | `drain_strategy and drain_interval are mutually exclusive` |
| Invalid resolver type | `resolver.type "x" is invalid; must be one of: rules, header_map, all, static` |
| Resolver default_binding not in route | `resolver.default_binding "x" not found in route bindings` |
| header_map missing header_key | `resolver.header_key is required for header_map type` |
| header_map empty | `resolver.header_map must have at least one entry` |
| header_map references unknown binding | `resolver.header_map["x"] references unknown binding "y"` |
| rules with no rules and no default | `resolver type "rules" requires at least one rule or a default_binding` |
| Rule references unknown binding | `binding_id "x" not found in route bindings` |
| Condition missing field | `field is required` |
| Condition invalid operator | `operator "x" is invalid` |
| Invalid regex pattern | `invalid regex pattern: ...` |
| Regex pattern too long | `regex pattern exceeds maximum length of 4096 characters` |

**Warnings** (non-fatal):
- `direct_hold` advisory: no inter-instance fencing, destination must handle duplicates
- `stale_claim_duration` too large relative to `step_down_grace`
