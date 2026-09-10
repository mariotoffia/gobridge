# Failover budget

What an exclusive session's failover actually costs, how the bridge admits a
declared objective against it, and what the two shipped lease profiles evaluate
to. Configuration fields live in
[Routes and runtime reference](routes-and-runtime-reference.md#routessession----route-session-management);
this page is the arithmetic behind them.

The endpoint of every figure here is **failure detection to successor
`ServiceLevelFull`** — the instant the successor is connected, subscribed and
serving, not the instant it wins the lease.

## Two failure modes, two formulas

A lease-bearing session can lose service in two unrelated ways, and one formula
cannot describe both.

**Owner death** — the process, the node, or its path to the lease store goes
away. Renewals stop, the lease expires, and a standby seizes it:

```
lease_ttl
+ 2 x max(1ms, ceil(1.25 x acquire_poll_interval))
+ (1 + ceil(lease_ttl / min_jittered_poll)) x renew_call_timeout
+ complete post-takeover transport activation
+ startup_allowance
```

The first poll establishes the post-response monotonic baseline; a later poll
quantizes the threshold crossing and immediately attempts takeover, so both
jittered poll boundaries are budgeted. Call latency after a successful
observation CAS is excluded from persisted elapsed and the manager waits only
after each Acquire call, so the budget counts the baseline call plus every
possible observation round at
`min_jittered_poll = max(1ms, poll - (poll/2)/2)`. Each complete Acquire shares
one `renew_call_timeout` across its internal store operations; the transport
activation capability is one aggregate bound, and connect, cleanup/replay,
recycle/reconnect, grace and final reconcile are **not** added again.

**Broker-path outage** — the active owner alone loses its path to the broker
while the lease store stays reachable. Renewals keep succeeding, so nothing
expires and none of the formula above applies. The timeline is anchored on the
step-down threshold instead:

```
broker_health_step_down
+ renew_interval + lease_renew_jitter/2                 (one detection round)
+ 2 x min(5s, step_down_grace)     (bounded source close, bounded lease release)
+ step_down_grace                                        (settlement grace)
+ 2 x max(1ms, ceil(1.25 x acquire_poll_interval))     (two standby polls)
+ 4 x renew_call_timeout                       (the store calls, listed below)
+ complete post-takeover transport activation
+ startup_allowance
```

The threshold is checked at the top of the renew timer case, so a crossing just
after a tick waits a full detection round — and the loop resets its timer only
*after* the body returns, so the round carries the store calls that body makes.
Four `renew_call_timeout`-bounded calls are budgeted in all: **two in the
detection round**, because once a renew streak has reached `max_renew_fails` the
body re-runs the authoritative ownership read on every later round, and a
node-local fault can degrade the store path along with the broker path; and
**two on the standby side**, because a standby whose Acquire is already in
flight when the release commits loses its observation compare-and-set, reads
that as ordinary contention, and waits a full poll before trying again — which
is also why two poll boundaries are budgeted, exactly as in the owner-death
formula. A released lease row has an empty owner, so the *winning* Acquire takes
over within its single call with no observation window to wait out; it is the
number of attempts that is two, not the calls inside one.

The owner that steps down does **not** compete for the lease it just released:
the step-down is terminal for that process, so the orchestrator restarts it and
it rejoins as a standby. Without that it would re-seize the row microseconds
after its own release — winning against a standby asleep for up to a full poll —
and take back a partition it has just proved it cannot serve. That restart is
**process-wide**: every route and session in the pod goes down with it, each
releasing its own lease cleanly on the way. Budget it under `startup_allowance`,
and weigh it when choosing a threshold — the cost of a false positive here is a
pod restart, which is why the threshold belongs comfortably above normal
reconnect-and-reconcile time.

Two things are deliberately outside the formula, on the same footing as the
owner-death formula's treatment of backend failure. A lease release the store
**refuses** leaves the row owned by an owner that has stopped renewing, so the
standby waits out the lease TTL and the owner-death path instead; that is a
lease-store failure and belongs to measured error-budget evidence, not to
admission. And the transport's **own** detection latency — an MQTT keepalive,
say — runs before the threshold clock starts, because the clock starts when the
transport reports the path lost.

## Admission

When `failover_slo` is present, **both** budgets must fit it, computed with
checked duration arithmetic before any store or transport is opened. The exact
boundary passes. The broker-path budget is only computed when
`broker_health_step_down` is a positive duration; when it is `off`, the
objective covers owner death alone — deliberately, and on the record.

Shared session IDs are first-wins at runtime, so preflight canonicalizes every
route/binding manager input per session and rejects any divergence in lease
cadence, SLO, startup, broker-path policy, or transport activation before
resources are opened. Route order is therefore irrelevant.

This is **admission control, not evidence**. It proves a configuration cannot
meet its objective; it never proves one does. See
[Measuring it](#measuring-it) before publishing any latency claim.

## The two derived lease profiles

A route session that pins neither `lease_ttl` nor `renew_interval` inherits one
of two baselines, chosen by deployment mode. Both rows below are evaluated at
the shipped defaults with the shipped MQTT (paho) transport at ITS defaults
(`connect_timeout`, `reconcile_timeout` and `unmatched_grace` all 30s, giving a
240s aggregate activation bound) and `startup_allowance: 0s`.

| Profile | `lease_ttl` | `renew_interval` | `renew_call_timeout` | `acquire_poll_interval` | `step_down_grace` | Owner-death budget | Broker-path budget |
|---|---|---|---|---|---|---|---|
| Standalone default (`deployment_mode: standalone`) | 360s | 75.56s derived | 5s derived | 5s derived | 15s | 1097.5s | `broker_health_step_down` + 382.5s |
| Clustered HA (`deployment_mode: clustered`, no pinned lease timing) | 45s | 10s | 3s | 5s derived | 5s | 336.5s | `broker_health_step_down` + 290s |

Two things an operator plans recovery around, and both were previously
invisible: the enforced bound is **three times the lease TTL** on the standalone
profile and **seven times** on the clustered one, because transport activation
and the observation-call budget dominate it; and the clustered profile is a
different, much shorter lease cadence that a page showing only the 360s default
never mentioned.

Shrink either budget by lowering the transport's `connect_timeout` /
`reconcile_timeout` / `unmatched_grace` (the 240s term above), by shortening
`lease_ttl`, or by raising `acquire_poll_interval` — a longer poll costs one
boundary but removes many observation calls.

The figures are derived, not maintained by hand: every build logs the same
numbers for its own configuration (`worst-case failover budget`, with
`broker_path_failover` stating where the second failure mode stands), and a test
checks this page against what the bridge discloses.

## Measuring it

An admitted configuration is a starting point. Before publishing a failover
claim, measure the endpoint the budget names — failure detection to successor
`ServiceLevelFull` — in the target deployment:

- **Warm and cold** samples separately. A cold successor pays a process start
  the warm one does not; budget that under `startup_allowance`, not as a
  surprise.
- **At least 20 samples per shape**, reported as p50/p95/p99/max, not as a
  single best case. Publish the p99 against the objective.
- **Under representative load.** An idle failover is not the one that happens.
- **Both failure modes**, if `broker_health_step_down` is on. Kill a process for
  owner death; isolate one member's broker path — leaving the lease store
  reachable — for the broker-path mode. Taking the broker down for every member
  proves the opposite thing.
- **`FailureToFullDuration`** is the metric to record; alert on it against the
  declared objective.

## The broker-path decision

`broker_health_step_down` is tri-state, and a declared `failover_slo` requires
one of the two explicit answers:

| Value | Meaning |
|---|---|
| omitted | The decision was not made. Refused when `failover_slo` is declared. |
| `off` | Broker-path failover is deliberately disabled; the objective covers owner death alone. |
| a positive duration | An active owner whose broker path stays non-converged that long releases the lease so a healthy standby takes over. |

Choose `off` when every node reaches the broker through one HA endpoint: a
*globally* unreachable broker would otherwise churn the lease between nodes that
all fail to connect, and each step-down costs a process restart. Choose a
duration — comfortably above normal reconnect+reconcile time — when a node can
lose its broker path alone, through routing, DNS, authorization or a security
group. See
[Broker-path failover](transports/mqtt.md#broker-path-failover-node-local-outage)
for the MQTT specifics and the `BrokerHealthStepDown` alert.
