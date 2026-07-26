# Running GoBridge as a cluster

This is the plain-language guide to running more than one GoBridge process
together, and to changing their configuration **without taking the service
down**. It is written for someone who has a single GoBridge running and is
wondering "what do I turn on, and what does each thing actually give me?"

If you operate a cohort day to day — posting changes and reading rollout
health — see [Operating a coordinated cohort](operating.md). For the manual
whole-cohort procedure that replacement-required changes need, see the
[operations runbook](../runbooks/cluster-config-rollout.md). To learn *how* the
coordinated rollout works under the hood, read the [protocol spec](spec/); for
what each mode costs, the [cost guide (TCO)](tco.md). This page is the "which
knob, and why" guide.

---

## The one problem this solves

A single GoBridge process re-reads its config and swaps to the new one on its
own — a live reload "just works", because there is only one of it.

The moment you run **several** processes that share the same brokers, the same
lease store, and the same durable queues, a config change becomes dangerous.
If each process picked up the change at a slightly different moment, for a few
seconds some would run the **old** config and some the **new** one. That split
can hand the same exclusive session to two owners, strand in-flight messages,
and generally corrupt exactly the durable state a cluster exists to protect.

So GoBridge gives you a ladder of four setups. Each rung costs a little more to
run and buys you a little more safety or convenience. Start at the bottom and
climb only as far as you need.

---

## The four setups at a glance

| Setup | You set | What it gives you | What it costs |
|---|---|---|---|
| **1. Standalone** | nothing (the default) | One process. Live config reloads apply immediately. | No high availability — if it dies, the bridge is down. |
| **2. Clustered (default)** | `deployment_mode: clustered` | Several processes share the load and cover for each other. | **Every** config change needs a full stop-and-restart of all processes (downtime). |
| **3. Coordinated** | `cluster.rollout: coordinated` + a roster + the DynamoDB config source | Safe config changes roll out across all processes with **no downtime**. | Needs the DynamoDB coordinated config store and a fixed list of members. |
| **4. Confirm window** | add `cluster.confirm_window: 90s` | A change that applies but **can't actually reach its broker** is rolled back automatically, on all processes. | A failed change disconnects twice (apply, then undo). Off by default. |

Setups 2–4 all require a clustered deployment. Setups 3 and 4 build on setup 2.

**On cost:** moving up the ladder barely changes your AWS bill — coordination is
a control-plane mechanism (one small DynamoDB table, polled every few seconds),
about **$1–2/month** on top of the availability baseline you already pay, flat
regardless of traffic. What each rung really changes is *operational* cost —
downtime per config change and the engineer time to run it. The full breakdown,
with reference scenarios and a decision matrix, is in the [cost guide](tco.md).

---

## Setup 1 — Standalone (the default)

Do nothing. One process, live reloads apply the instant you write the new
config. This is the right choice until you need a second process for
availability or throughput.

There is no `cluster` section, and `deployment_mode` is left unset.

---

## Setup 2 — Clustered, changes refused by default

Turn a deployment into a cohort by marking it clustered:

<!-- docs-example: skip -->
```yaml
bridge:
  deployment_mode: clustered
```

Now you can run several processes against the same brokers and stores, and they
will cover for one another. But because they share durable state, GoBridge
**refuses any live config change by default** and tells you so — it will not let
you create the mixed-version split described above.

To change config in this setup you must do a **whole-cohort replacement**: stop
every process, write the new config, start every process. That is a short,
planned outage. The [operations runbook](../runbooks/cluster-config-rollout.md#procedure)
has the exact procedure.

This is the safe, simple default ([ADR 0012](../adr/0012-cluster-config-whole-cohort-replacement.md)).
If you rarely change config, or a brief outage per change is acceptable, stop
here — you do not need anything below.

---

## Setup 3 — Coordinated rollout (no-downtime changes)

If you change config often and want to avoid the outage, turn on **coordinated
mode**. Now a safe change is rolled out to the whole cohort atomically: it is
proposed to a shared store, **every** process validates and prepares it, and
only once all of them agree does a single elected process ("the coordinator")
commit it — at which point they all swap together. If any process cannot accept
the change, nothing swaps and the old config keeps running. No process ever runs
a config the others have not agreed to ([ADR 0013](../adr/0013-coordinated-cluster-config-rollout.md)).

### What you need first

Coordinated mode has three prerequisites — all of them, or it will not start:

1. **The versioned DynamoDB config source.** The shared store that can hold the
   config and version it (the `dynamodb_coordinated_ha` deployment profile). A
   file- or EFS-based cohort cannot use coordinated mode.
2. **A member roster** — the fixed list of process identities in the cohort.
3. **A stable `member_id` per process** — each process announces an id that must
   appear in the roster and must survive restarts. This is set in the
   deployment/bootstrap config (`member_id`), not in the shared logical config,
   because every process shares one config document.

### What you set

In the shared logical config:

<!-- docs-example: skip -->
```yaml
bridge:
  deployment_mode: clustered
  cluster:
    rollout: coordinated          # the default is "refuse" (setup 2)
    members: [node-a, node-b, node-c]   # every process, by its member_id
```

And in each process's deployment/bootstrap config, its own identity:

<!-- docs-example: skip -->
```yaml
member_id: node-a     # must be one of bridge.cluster.members; stable across restarts
```

That is all. The shipped image performs the rollout itself — there is nothing to
run per change beyond writing the new config. Post it to the config source and
the cohort rolls it out.

### One important limit

Coordinated mode only rolls out **live-safe** changes — the same kinds of change
a single process is allowed to reload live (routing, bindings, processor tuning,
log level, non-identity session options).

Changes that touch **durable identity or storage** — a session's client id or
subscription, a lease/outbox/DLQ **store target**, the `deployment_mode`, or the
cohort's own `members` / `endpoints` / `rollout` settings — are
**replacement-required**: they still need the whole-cohort stop-and-restart from
setup 2, even in a coordinated cohort. GoBridge classifies the change for you and
refuses to roll a replacement-required one through the barrier, naming the reason.
See [which changes are live-safe](operating.md#which-changes-roll-live-and-which-need-a-window).

---

## Setup 4 — Confirm window (auto-revert on failure)

Coordinated mode commits a change once every process has **built** it — proved
it is valid and can be prepared. But "valid" is not the same as "actually
working": a config can be perfectly well-formed and still fail to reach its
broker (a wrong credential, a denied topic, an unreachable endpoint). By
default such a change still commits, and the affected process raises an alarm
for an operator to handle.

The **confirm window** raises the bar from "built" to "actually connected".
Turn it on by adding a duration:

<!-- docs-example: skip -->
```yaml
bridge:
  cluster:
    rollout: coordinated
    confirm_window: 90s     # unset / "0s" (the default) = no confirm window
```

Now a commit is **provisional**. Every process swaps to the new config and
starts a countdown. Each process that genuinely connects to its broker reports
"converged". If the **whole** cohort converges before the window ends, the
coordinator makes the change permanent. If **any** process fails to converge in
time — or the coordinator itself dies — every process automatically rolls back
to the previous config when its countdown runs out. The cohort reverts together;
it is never left split. A process that crashes during the window reboots on the
**last confirmed** config, never the one on trial
([ADR 0014](../adr/0014-confirm-window-provisional-commit.md)).

This is the NETCONF / Cisco NSO "commit confirmed" idea: apply on trial, and
undo by doing nothing if it does not prove itself in time.

### The trade-off (why it is off by default)

A change that fails the trial disconnects your sessions **twice** — once to
apply it, once to undo it. For exclusive-identity MQTT sessions that is two
reconnect storms instead of one. So turn the confirm window on only for changes
where a broken-but-valid config staying live is worse than a second reconnect.
Leave it unset otherwise.

`confirm_window` is only valid together with `rollout: coordinated`.

---

## Choosing your rung

- Just one process is fine → **Standalone**.
- Need availability/throughput, rarely change config → **Clustered (default)**.
- Change config often, want zero-downtime changes → **Coordinated**.
- Also want automatic rollback when a change can't reach its broker → **Confirm
  window**.

### Scenarios — which mode fits you

Find the row closest to your situation:

| Your situation | Mode | Why |
|---|---|---|
| A dev/test bridge, or a single low-stakes process where an occasional restart is fine | **Standalone** | No coordination to pay for or set up; live reload just works. |
| You run two or more processes so a crash isn't an outage, and you change config **rarely** (planned, a few times a year) | **Clustered (refuse)** | Cheapest HA option. A short maintenance window a few times a year is acceptable, so you don't need the barrier. |
| An HA cohort you retune **often** (routes, bindings, processors, log level) and a maintenance window each time is painful | **Coordinated** | Live-safe changes roll out with zero downtime; the extra infra cost is ~$1–2/month. |
| You push config through CI or the admin API and want fleet-wide, no-downtime rollouts | **Coordinated** | Post once to the config source; the cohort proposes, all-acks, and swaps itself. |
| Your changes sometimes touch **credentials, broker ACLs/topics, or endpoints** that could validate but fail to connect | **Confirm window** | A change that can't reach its broker auto-reverts the whole cohort instead of latching a degraded alarm. |
| A change-averse or regulated setting where a bad change must roll back **automatically, without paging anyone** | **Confirm window** | Abort-by-inaction: if the cohort doesn't converge in the window, it reverts on its own. |
| Routine tuning that essentially always connects successfully | **Coordinated** (window off) | Avoid the confirm window's double-disruption on the rare failure — you don't need it when changes reliably converge. |

For the money and effort behind each of these, see the [cost guide](tco.md).

You can move up the ladder at any time; each step is additive. Moving between
rungs is itself a `deployment_mode` / `cluster.*` change, which is
replacement-required — plan it as a whole-cohort replacement.

## See also

- [Operating a coordinated cohort](operating.md) — posting changes, reading
  rollout health, and handling an abort, in plain language.
- [Cost guide (TCO)](tco.md) — what each mode costs in dollars and operational
  effort, with reference scenarios and a decision matrix.
- [Operations runbook](../runbooks/cluster-config-rollout.md) — the manual
  whole-cohort replacement procedure and stuck-rollout recovery.
- [Protocol spec](spec/) — the design, the failure matrix, and which parts of
  NETCONF, Raft, ZooKeeper, Envoy xDS, Kafka and others GoBridge reuses.
- [ADR 0012](../adr/0012-cluster-config-whole-cohort-replacement.md),
  [0013](../adr/0013-coordinated-cluster-config-rollout.md),
  [0014](../adr/0014-confirm-window-provisional-commit.md) — the decisions and
  why the alternatives were rejected.
