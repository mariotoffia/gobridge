# 0007 — Cluster worker seeding: AdoptValid default

Status: superseded
Superseded by: 0012
Date: 2026-07-03
Deciders: GoBridge core

## Overview

This historical record explains why workers never write shared config.
[ADR 0012](0012-cluster-config-whole-cohort-replacement.md) supersedes its
cluster reconfiguration rule, with
[ADR 0013](0013-coordinated-cluster-config-rollout.md) allowing coordinated
live-safe deltas. The seeder modes described by the original decision are
removed. The current startup rule is
[strict initial configuration](../aws-deployment/config-initialization.md):
only control may create an absent target; workers remain read-only.

## Context

The original file-based cluster kept `bridge.yaml` on shared EFS. Its
deployment seeder and admin commits could produce different content: after an
admin edit, the live file no longer matched the synth-time asset hash.

Worker tasks start from that same EFS config. If a worker seeded or overwrote
EFS on startup, a rolling worker deploy would stomp an Admin-API commit back to
the synth-time asset, and concurrent workers writing the same file during a
rolling deploy would race. The question is what a worker does with an EFS config
that is valid but drifted from the asset it shipped with.

## Historical decision

Workers adopted current valid EFS config rather than overwriting it.
`AdoptValid` was the read-only worker default; `AbortDeploy` was a strict
asset-match gate. A separate seeder implemented those startup checks.
The single control service and read-only worker mount protected the file
against competing writes.

Those modes are historical, not supported configuration options.

## Current decision

The bridge process owns optional initialization. An embedded logical document
may create an absent target through `ports.ConfigInitializer.CreateIfAbsent`,
at version 1. It never replaces a present document, including an invalid or
versionless one, and it rereads the winner after a creation race. There is no
seeder image or S3 config download dependency.

Workers remain read-only for both file and DynamoDB config. With valid
bootstrap, absence leaves the control plane live but not ready and the data
plane idle. After clustered activation, confirmed absence stops intake and
requires process exit and replacement, not a same-process transition to idle.
Standalone runtimes can return to idle after safe quiescence; uncertain
teardown also exits. Read errors retain the last successful runtime as degraded.

First activation is not readiness: a standby may activate without reaching
Full. Same-process idle never rearms initialization. A fresh process may create
an absent target again; there is no durable tombstone.

## Consequences

- Existing admin changes survive initialization attempts and worker replacement.
- Updating embedded config does not update the shared target.
- Missing config and unreadable config have different processing consequences.
- Startup creation cannot serve as a cluster rollout mechanism. Follow the
  [cluster rollout procedure](../runbooks/cluster-config-rollout.md).

## Alternatives considered

- **Workers seed/overwrite EFS on startup.** A rolling worker deploy would
  overwrite a live Admin-API commit with the synth-time asset, and concurrent
  workers would race on the write. Rejected — it makes hot reconfiguration and
  rolling deploys mutually destructive.
- **Asset-match gate as the default.** It would strand replacement workers
  after valid admin edits. The historical opt-in gate is removed as well.
- **Let workers mount EFS RW read-only by convention.** Convention is not
  enforcement. The read-only mount plus the IAM scope make a worker write
  unavailable to the deployed worker role.
