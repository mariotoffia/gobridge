# Operating a coordinated cohort

This is the day-to-day guide to changing the config of a cohort running in
**coordinated** mode (setups 3 and 4 in the [configuration guide](README.md)) —
in plain language, no downtime, no manual steps.

If your change is **replacement-required**, or your cohort is **not** in
coordinated mode, changing config is a manual stop-and-restart instead — see the
[whole-cohort replacement procedure](../runbooks/cluster-config-rollout.md).

---

## Making a change

Write the new config **once** — a single durable write to the config source, or
the admin commit API. Do **not** stop anything, and do not stage-and-restart
(that is the whole-cohort path).

Every process sees the change through its own config source, checks that it is
live-safe, proposes it to the cohort, and holds its own swap until the cohort
agrees. When every member has validated and built the change, it commits and all
members swap together — with no downtime. For a live-safe change, that is the
entire job: post it and watch it land.

## Watching it roll out

Deep health (`GET /api/v1/monitor/deephealth`, under `config_watch.rollout`)
shows the rollout as this member last saw it:

- **`state`** — `proposed` → `staging` → `committed` (or `aborted`).
- **`config_version`**, the frozen member roster (**`epoch`**), and who has
  **`acked`** / **`nacked`**.
- **`reconfigure_pending`** — expected `true` until the change commits: the
  member is deliberately not applying a config the cohort has not agreed to.

**The roster minus who has acked (minus who nacked) is exactly who the cohort is
waiting on.** A rollout that seems slow names the member holding it up — most
often one whose own config source has not yet delivered the change.

If you set a **confirm window**, the commit is a trial: after it commits, each
member swaps and the cohort must actually connect (converge) to its brokers. The
state stays `committed` until the whole cohort converges, then reads `confirmed`.
If the cohort cannot converge before the window ends, it reverts on its own and
reads `reverted`.

## When a change doesn't go through

- **Aborted** — a member could not build the change, or the cohort did not all
  acknowledge before the deadline. **Nothing swaps**, every member keeps its
  running config, and `state` reads `aborted` with a `reason`. Fix the config and
  re-post it, or leave it — the cohort is unharmed.
- **A member restarts during or after an abort** — it boots on the last
  **committed** config, not the rejected candidate; it never joins the cohort on
  a config no peer runs. To clear a lingering `reconfigure_pending`, roll the
  config source back to the last committed document (or fix and re-propose the
  change).

## Which changes roll live, and which need a window

Coordinated mode rolls only **live-safe** changes — the ones a single process is
allowed to reload live. Everything else is **replacement-required** and needs the
[whole-cohort replacement procedure](../runbooks/cluster-config-rollout.md), even
in a coordinated cohort:

| Class | Examples | How it is applied |
|---|---|---|
| **live-safe** | routing/binding changes, processor tuning, log level, non-identity session options, adding/removing a non-durable route | Coordinated barrier (no downtime) |
| **replacement-required** | changing a durable session identity (client id, subscription); changing a lease / outbox / DLQ **store target**; changing `deployment_mode` | Whole-cohort replacement |
| **replacement-required (cohort shape)** | changing `bridge.cluster.members` (the roster), `bridge.cluster.endpoints`, or `bridge.cluster.rollout` itself | Whole-cohort replacement |

The last row is the one operators most often miss: a coordinated cohort **cannot
roll a change to its own roster, endpoint map, or rollout mode through the
barrier** — the roster is the membership the barrier freezes and counts
acknowledgements against, so changing it is structurally a whole-cohort
replacement. The cohort refuses such a change up front (it is never proposed) and
names the class.

## See also

- [Configuration guide](README.md) — which mode to run and what each one gives you.
- [Whole-cohort replacement procedure](../runbooks/cluster-config-rollout.md) —
  the manual stop-and-restart for replacement-required changes and non-coordinated
  cohorts, plus recovering a stuck rollout.
- [Cost guide (TCO)](tco.md) · [Protocol spec](spec/)
