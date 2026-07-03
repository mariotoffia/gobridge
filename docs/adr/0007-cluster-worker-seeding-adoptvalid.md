# 0007 — Cluster worker seeding: AdoptValid default

Status: accepted
Date: 2026-07-03
Deciders: GoBridge core

## Context

The file-based cluster keeps its `bridge.yaml` on a shared EFS volume. Two
reconfiguration paths write to it: the CDK synth-time seed (the config baked into
the task definition at deploy) and the Admin API commit (a hot reconfiguration an
operator applies to the running control task). Both are legitimate, and they
diverge — after an Admin-API commit, the live EFS config no longer matches the
hash of the synth-time asset.

Worker tasks start from that same EFS config. If a worker seeded or overwrote
EFS on startup, a rolling worker deploy would stomp an Admin-API commit back to
the synth-time asset, and concurrent workers writing the same file during a
rolling deploy would race. The question is what a worker does with an EFS config
that is valid but drifted from the asset it shipped with.

## Decision

Workers adopt the current valid EFS config instead of seeding or overwriting it.
The seeder on the control task is the only read-write writer of EFS. Design in
`deployment/aws-filebased-config/cdk`.

- **AdoptValid is the worker default.** `ModeWorker` runs the seeder in
  `MODE=AdoptValid`
  (`constructs/internal/gobridgebase/base.go:37`, default at
  `defaultSeederMode:519`). Worker startup gates on the current EFS config being
  present and parseable, but tolerates hash drift from the synth-time asset
  (`base.go:37-42`, `:194-200`), so Admin-API hot reconfiguration and worker
  self-healing coexist.

- **Workers never write EFS.** A worker mounts EFS read-only at the ECS volume
  layer — the main-container mount sets `ReadOnly` whenever the construct runs
  in `ModeWorker` (`gobridgebase/base.go:445`). The read-only worker modes
  (`AdoptValid` / `AbortDeploy`) stage under `/tmp` and only read
  `dirname(EFS_TARGET_PATH)`; the worker task role is granted no
  `ClientWrite` EFS action (`base.go:377-383`).

- **The seeder is the sole RW writer.** Only the seeder mounts EFS RW
  (`base.go:383`, `ReadOnly: false`). The control service is pinned to
  `DesiredCount = 1` with `MinHealthyPercent=0 / MaxHealthyPercent=100`, so the
  previous control task fully drains before the next starts — no two RW writers
  touch EFS at once during a rolling deploy (`cluster.go:175-181`).

- **AbortDeploy is opt-in strict lock-step.** Set `WorkerSeederMode =
  "AbortDeploy"` (`base.go:194-200`, `cluster.go:137-145`) for deployments that
  require every worker to run the exact synth-time asset — a worker then refuses
  to start on any drift.

## Consequences

- An Admin-API commit survives a rolling worker deploy. Workers adopt the live
  EFS config rather than reverting it to the synth-time asset.
- The two reconfiguration paths coexist: CDK seeds the initial config, the Admin
  API mutates it live, and workers follow whatever is currently valid on EFS.
- Only one writer ever touches EFS, so there is no write-write race on
  `bridge.yaml` during a deploy. This is enforced by the RO worker mount, the IAM
  scope, and the single-control-task drain — three independent guards.
- A worker will not start on an EFS config that is missing or unparseable. A
  corrupt config fails the worker fast rather than running stale.
- Teams that need lock-step deploys give up the coexistence and opt into
  `AbortDeploy`, accepting that an Admin-API commit then blocks new workers until
  the asset is re-synthed to match.

## Rejected alternatives

- **Workers seed/overwrite EFS on startup.** A rolling worker deploy would
  overwrite a live Admin-API commit with the synth-time asset, and concurrent
  workers would race on the write. Rejected — it makes hot reconfiguration and
  rolling deploys mutually destructive.
- **AbortDeploy as the default.** Safe but rigid: every Admin-API commit would
  strand new workers until a re-synth. Offered as opt-in for teams that want it,
  not imposed on the common case.
- **Let workers mount EFS RW read-only by convention.** Convention is not
  enforcement. The read-only mount plus the IAM scope make a worker write
  impossible, not merely discouraged.
