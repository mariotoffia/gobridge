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
| **3. Independent** | `cluster.rollout: independent` | Safe config changes apply with **no downtime** and **nothing extra to run** — each process picks the change up and applies it itself, exactly as a standalone bridge does. | For a few seconds one process can be running the new config while another is still on the old one. A process that cannot run the change is a broken process to replace, not a veto. |
| **4. Coordinated** | `cluster.rollout: coordinated` + a roster + the DynamoDB config source | Safe config changes roll out across all processes with **no downtime**, and a process that cannot build the change stops it reaching any of them. | Needs the DynamoDB coordinated config store and a fixed list of members. |
| **5. Confirm window** | add `cluster.confirm_window: 90s` | A change that applies but **can't actually reach its broker** is rolled back automatically, on all processes. | A failed change disconnects twice (apply, then undo). Off by default. |

Setups 2–5 all require a clustered deployment. Setups 3, 4 and 5 build on setup 2;
4 and 5 build on each other, 3 stands alone.

**Choosing between 3 and 4.** Setup 3 is what most deployments want and is the
cheaper thing to operate: nothing to provision, nothing to elect, nothing that can
get stuck. Take setup 4 when a config that one process cannot build must not reach
any of them — for example when the processes do not run identical images, or when
a half-applied change would be worse than no change at all. Both refuse the same
set of changes that cannot be applied live at all, and both name the reason.

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

## Setup 3 — Independent (no downtime, nothing extra to run)

If you want to change config without the outage and without running a
coordination protocol, say so:

<!-- docs-example: skip -->
```yaml
bridge:
  deployment_mode: clustered
  cluster:
    rollout: independent
```

Now a safe change applies the way it does on a standalone bridge: whatever writes
the config — the admin HTTP API, or an edit to the shared document — validates it
once, and each process then picks it up and applies it itself. There is no shared
store to provision, no coordinator to elect, no roster to keep in step, and
nothing that can get stuck waiting for a process that is not answering.

**What you are accepting.** The processes do not switch at the same instant. For a
few seconds one can be running the new config while another is still on the old
one — the same window a rolling Kubernetes ConfigMap update has. If a change would
be harmful half-applied, use setup 4 instead.

**A process that cannot run the change does not stop the others.** It fails to
apply, keeps serving its previous config, and reports the failure on its health
endpoint (`config_watch.degraded` with the reason). It is a process to replace,
not a veto over the cohort. Watch for it the way you watch for any unhealthy
process.

**What is still refused.** A change that cannot be applied live on *any* single
process — a durable session's identity, a store's target — is refused here too,
with the same message a standalone bridge gives, and still needs the whole-cohort
replacement from setup 2.

---

## Setup 4 — Coordinated rollout (no-downtime, nobody swaps alone)

If you change config often and want to avoid the outage, turn on **coordinated
mode**. A safe change is now *decided* atomically: it is proposed to a shared
store, **every** process validates and prepares it, and only once all of them
agree does a single elected process ("the coordinator") commit it. If any process
cannot accept the change, nothing swaps and the old config keeps running. No
process ever runs a config the others have not agreed to
([ADR 0013](../adr/0013-coordinated-cluster-config-rollout.md)).

The *decision* is atomic; the *swap* is per process. After the commit each member
applies the change locally, and one of them can fail where the others succeed —
so for a few seconds (and, if a member's broker or store is unhealthy, longer)
the cohort can be running two generations. That window is bounded and visible,
not hidden: the failing member retries, reports `applied: false` in deep health,
and eventually declares itself unrepairable so you can replace it. Alarm on it —
see [Watching it roll out](operating.md#watching-it-roll-out). If you cannot
tolerate that window at all, use a [confirm window](#setup-4--confirm-window-auto-revert-on-failure), which
reverts the whole cohort instead of leaving it split.

### What you need first

Coordinated mode has three prerequisites — all of them, or it will not start:

1. **A shared rollout store.** A DynamoDB table holding the current proposal, the
   per-member acknowledgements, and the durable last-committed config artifact.
   The `dynamodb_coordinated_ha` deployment profile provisions it; the config
   document itself may still live on EFS.
2. **A member roster** — the fixed list of process identities in the cohort.
3. **A stable `member_id` per process** — each process announces an id that must
   appear in the roster and must survive restarts. This is set in the
   deployment/bootstrap config (`member_id`), not in the shared logical config,
   because every process shares one config document.

Prerequisite 3 is what decides the deployment shape. A process needs an identity
that is the same after a restart, so an autoscaled pool cannot host a cohort:
every replacement task is a new task with a new id, so it can never re-enter the
roster it left. The AWS profile therefore runs a coordinated cohort as **static
member slots** — one single-task ECS service per roster member, each with its own
`member_id` — and **rejects `rollout: coordinated` at synth time** on its
autoscaled worker shape rather than deploying a stack that can only fail at boot.
See [the AWS deployment configuration reference](../aws-deployment/configuration.md).

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

And in each process's deployment/bootstrap config, its own identity. In the AWS
file-based profile that config is the JSON document supplied through
`GOBRIDGE_FILEBASED_BOOTSTRAP_JSON` — one per member, differing only in
`member_id` and `node_role`:

<!-- docs-example: skip -->
```json
{
  "bridge_id": "gobridge-prod",
  "config_file_path": "/var/lib/gobridge/bridge.yaml",
  "admin_api_key_param": "/gobridge/prod/admin-api-key",
  "topology": "dynamodb_coordinated_ha",
  "node_role": "worker",
  "member_id": "node-a",
  "dynamodb_ha_lease_table_name": "gobridge-prod-leases",
  "dynamodb_ha_outbox_table_name": "gobridge-prod-outbox",
  "dynamodb_ha_managed_subscriptions_table_name": "gobridge-prod-managed-subscriptions",
  "dynamodb_ha_rollout_table_name": "gobridge-prod-rollouts",
  "dynamodb_ha_config_fingerprint": "0000000000000000000000000000000000000000000000000000000000000000",
  "dynamodb_ha_baseline_config_digest": "0000000000000000000000000000000000000000000000000000000000000000"
}
```

`member_id` must be one of `bridge.cluster.members` and must be stable across
restarts; it and `node_role` are the only two values that differ between the
members. Everything below them is deployment-owned identity the CDK construct
computes and stamps for you — the two digests are SHA-256 hex values over the
admitted config, not operator-chosen strings, so the zeros above are a shape
placeholder. Each field is described in the
[bootstrap field reference](../aws-deployment/configuration.md#field-reference).

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

## Setup 5 — Confirm window (auto-revert on failure)

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
