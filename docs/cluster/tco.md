# Cluster mode — total cost of ownership

This guide helps you weigh the **four cluster setups** (see the
[configuration guide](README.md)) in cost terms, so you can pick one on
evidence rather than instinct. It answers three questions:

1. How much does each mode add to my AWS bill? (Very little — read on.)
2. What does each mode cost me *operationally* — downtime, engineer time, risk?
3. Where is the crossover — when does moving up a rung pay for itself?

For the absolute AWS pricing of the underlying deployment (Fargate, EFS,
DynamoDB, networking), see the [AWS deployment TCO](../aws-deployment/tco.md);
this guide only covers the **delta** each cluster mode adds on top of it. Rates
quoted here match that doc: **us-west-1 on-demand** (`$0.25`/M read units,
`$1.25`/M write units, a Standard 0.5 vCPU / 1 GiB Fargate task ≈ `$18`/month).

---

## The honest summary

**The cluster mode you choose barely changes your infrastructure bill.** The
number of processes you run (for availability and throughput) and your message
throughput dominate the bill — and those are the *same* whether you refuse live
changes or coordinate them. Coordination is a **control-plane** mechanism: it
runs once per config change, not per message.

So the real cost axis is **operational**:

| Mode | Infra delta vs the HA baseline | Operational cost |
|---|---|---|
| **Standalone** | — (one process, no coordination stores) | None to change config (live reload). But **no availability** — a crash is an outage. |
| **Clustered (refuse)** | The HA baseline: N processes + lease/outbox/config DynamoDB. **No rollout-specific cost.** | **A maintenance window on every config change** (stop all → change → start all), plus the engineer time to run it safely. |
| **Coordinated** | **~$1–2/month** for one small `gobridge-rollouts` table, polled every 2 s. Flat; independent of throughput. | **~Zero.** Live-safe changes roll out with no downtime and no manual procedure. One-time setup effort to enable. |
| **Confirm window** | **~$0 extra** (a few more requests during each trial window). | A failed trial disconnects **twice** (apply + revert). In exchange, a non-converging change **auto-recovers** with no human. |

The takeaway: **coordinated mode is essentially free to run** (a dollar or two a
month over the HA baseline you already pay), and it *removes* operational cost.
The only real price is the one-time work to enable it.

---

## Infrastructure cost, mode by mode

### Standalone

One process. No shared stores, no coordination. Cost is just that single task
(≈ `$4`/month on Spot for dev, ≈ `$18`/month on-demand for a Standard task) plus
whatever transports/stores its routes use. There is nothing cluster-specific to
pay for.

### Clustered (refuse) — the HA baseline

Running a cohort for availability means N processes and the shared stores they
coordinate through — a DynamoDB lease store, and (for exclusive MQTT / shared
outbox) the outbox and managed-subscription tables. This is the
[`dynamodb_coordinated_ha` reference cluster](../aws-deployment/tco.md#production-cluster-130--200month),
≈ `$54`/month of compute for 1 control + 2 worker tasks, plus the DynamoDB
request cost that tracks your throughput.

**None of that is a rollout cost** — you pay it purely for availability. Refuse
mode adds *nothing* on top: it is the default, with no rollout table and no
coordination polling.

### Coordinated — one small table, polled

Coordinated mode adds exactly one thing: the **`gobridge-rollouts`** DynamoDB
table. It holds **two singleton rows** — `ROLLOUT#current` (the active rollout)
and `ROLLOUT#committed` (the durable last-committed config artifact). On-demand,
so you pay per request; storage is two rows well inside the free tier.

The ongoing cost is **polling**: every member re-reads the rollout row every
`2 s` (the `defaultRolloutPollInterval`) to observe state changes. That is the
only continuous charge, and it is tiny and flat:

```text
Poll reads (N=3 cohort, 2 s interval, strongly-consistent reads of a <1 KB row):
  3 members x (86,400 s / 2 s) = 129,600 reads/day
  129,600 x 30 days            = ~3.9M reads/month
  ~3.9M x (1-2 RRU) x $0.25/M  = ~$1-2/month
```

Writes happen **only when config actually changes** — a handful of conditional
writes per rollout (propose, each member's ack, the commit, the artifact write).
Even a cohort that changes config many times a day spends pennies on rollout
writes. Storage is negligible.

**Total coordinated delta: ~$1–2/month, flat, regardless of message throughput.**
You can lower it further — see [Tuning the coordination cost](#tuning-the-coordination-cost).

### Confirm window — negligible extra

The confirm window reuses the same table and polling loop. During a trial window
each member additionally writes one `Converge` record and the coordinator writes
one `Confirmed`/`Reverted` — a few extra request units per rollout. There is **no
new table and no extra steady-state polling**, so the infrastructure delta over
coordinated mode is effectively **$0**.

Its cost is not on the AWS bill — it is the **double disruption** on a failed
trial (the change applies, then reverts), which matters only for exclusive-
identity MQTT sessions and only when a trial actually fails.

---

## Operational cost — the axis that matters

### Refuse mode: a window per change

Without coordination, every config change to a cohort is an externally
coordinated **whole-cohort replacement**: quiesce ingress, drain and stop **all**
members, write the config, start them, verify the version/readiness barrier on
every member, re-enable ingress (the
[operations runbook](../runbooks/cluster-config-rollout.md#procedure) has the
full procedure). Budget:

- **Downtime:** the drain-to-restart-to-ready window — minutes to tens of
  minutes, during which the cohort is not serving.
- **Engineer time:** a careful, checklist-driven operation each time, with a
  whole-cohort rollback plan if a member fails to come up.
- **Risk:** a manual procedure is a chance to get it wrong under time pressure.

Multiply that by how often you change config. A cohort you retune weekly pays
this ~52 times a year.

### Coordinated mode: post and walk away

A live-safe change is a single durable write to the config source; the cohort
proposes, all-acks, commits, and swaps itself with **no downtime and no manual
procedure**. The operational cost per change drops to roughly zero.

### The crossover

Because coordinated mode's marginal infra cost is ~$1–2/month, the crossover is
almost immediate:

> Coordinated mode pays for itself the **first time** you would otherwise take a
> maintenance window for a live-safe change. After that it is pure savings.

The only real investment is the **one-time setup**: move the cohort to the
versioned DynamoDB config source, define the `bridge.cluster.members` roster, and
give each task a stable `member_id`. If you change config more than very rarely,
that setup is cheaper than the windows it eliminates.

### When the confirm window earns its double-disruption

The confirm window trades a guaranteed *second* reconnect on a failed trial for
**automatic recovery without a human**. It is net-positive when:

```text
  P(change fails to converge) x cost of a bad config staying live until a human reacts
      >
  P(change fails to converge) x cost of one extra reconnect
```

i.e. whenever *a non-converging config staying active is worse than a second
reconnect* — credential rotations, new broker ACLs/topics, endpoint changes that
might not reach the broker. For routine routing/processor tuning that almost
always converges, leave it off and avoid the double-disruption risk entirely.

---

## Reference scenarios

Realistic profiles, framed the way the
[AWS reference architectures](../aws-deployment/tco.md#reference-architectures)
are. Compute/DynamoDB baselines are cross-referenced there; the **rollout delta**
is the cluster-specific part.

### A. Dev / single bridge — Standalone

One task, changes config freely.

| Line | Cost |
|---|---|
| Compute (1 Spot task) | ~$4/month |
| Rollout coordination | $0 (none) |
| Operational cost per change | $0 (live reload) |
| **Total** | **~$4/month** |

Use it until you need a second process for availability.

### B. HA pair, config changes rarely — Clustered (refuse)

Availability matters; config changes a few times a year on a planned window.

| Line | Cost |
|---|---|
| Compute (HA cluster baseline) | ~$54/month |
| DynamoDB stores (lease/outbox, throughput-driven) | see [AWS TCO](../aws-deployment/tco.md#dynamodb-store-costs) |
| Rollout coordination | $0 (refuse mode) |
| Operational cost | ~1 maintenance window per change |
| **Total** | **~$54/month** + a handful of windows/year |

If a short planned outage a few times a year is acceptable, this is the cheapest
HA option — don't pay setup effort you won't recoup.

### C. HA MQTT, retuned weekly, no downtime allowed — Coordinated

Same cohort, but you change routing/processors often and cannot take windows.

| Line | Cost |
|---|---|
| Compute (HA cluster baseline) | ~$54/month |
| Rollout coordination (`gobridge-rollouts`, 2 s poll) | ~$1–2/month |
| Operational cost per change | ~$0 (no downtime, no procedure) |
| **Total** | **~$55–56/month** |
| **vs refuse** | +~$2/month infra, **−~52 maintenance windows/year** |

The ~$2/month buys away a year of maintenance windows. Clear win at this change
frequency.

### D. HA MQTT with risky changes — Confirm window

As C, but changes sometimes touch credentials, broker ACLs, or endpoints that
might not connect.

| Line | Cost |
|---|---|
| Compute (HA cluster baseline) | ~$54/month |
| Rollout coordination + confirm window | ~$1–2/month |
| Operational cost, successful change | ~$0 |
| Operational cost, **failed** trial | one extra reconnect, then auto-revert (no human) |
| **Total** | **~$55–56/month** |

Same bill as C; you spend a second reconnect only when a change would otherwise
have latched a broken generation waiting for a human.

---

## Decision matrix

| Criteria | Standalone | Clustered (refuse) | Coordinated | Confirm window |
|---|---|---|---|---|
| Need availability / HA | No | Yes | Yes | Yes |
| Config changes | Any frequency | Rare / planned | Frequent | Frequent + risky |
| Downtime per change | None | A window | None | None (2× on failed trial) |
| Infra delta | — | HA baseline | +~$1–2/mo | +~$1–2/mo |
| One-time setup | None | Shared stores | Stores + roster + `member_id` | Same as coordinated |
| Auto-rollback of a bad change | n/a | No | No (alarm only) | **Yes** |

**Recommendation:** start at the lowest rung that meets your availability and
change-frequency needs. Reach for **coordinated** as soon as maintenance windows
become a recurring annoyance — the infra cost is noise. Add the **confirm
window** only for the subset of changes that can validate yet fail to connect.

---

## Tuning the coordination cost

The coordination bill is already small, but if you run a large cohort or want to
trim it:

- [ ] **Slow the poll interval.** It defaults to 2 s and is a composition-root
  setting (`ClusterRolloutConfig.PollInterval`), not a `bridge.yaml` key.
  Doubling it to 4 s halves the polling read cost, at the price of detecting a
  rollout state change a little later; for control-plane changes that resolve
  over seconds, a slower poll is usually fine.
- [ ] **Keep the cohort small.** Polling cost scales linearly with member count;
  the barrier's operator contract targets cohorts of ≲ 10 anyway.
- [ ] **Leave the confirm window off** unless a change class needs it — it adds
  the double-disruption failure cost for no benefit on changes that always
  converge.
- [ ] **Right-size compute first.** Coordination is a rounding error next to the
  per-task Fargate cost — the biggest lever on a cluster bill is task count and
  size, covered in the [AWS deployment TCO](../aws-deployment/tco.md#fargate-compute).

---

## Related guides

| Guide | Description |
|---|---|
| [Cluster configuration guide](README.md) | Which mode to choose and what each one gives you, in plain language. |
| [Protocol spec](spec/) | The design behind coordinated rollout and the confirm window. |
| [Operations runbook](../runbooks/cluster-config-rollout.md) | Posting changes, reading rollout health, the whole-cohort procedure. |
| [AWS deployment TCO](../aws-deployment/tco.md) | Absolute AWS pricing for compute, storage, DynamoDB, and networking. |
| [AWS deployment overview](../aws-deployment/overview.md) | Cluster topologies (`GoBridgeCluster` vs `GoBridgeDynamoDBHA`) and CDK constructs. |
