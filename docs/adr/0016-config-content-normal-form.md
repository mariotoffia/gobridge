# 0016 — Configuration content identity: compare the normal form, not the bytes

Status: accepted
Date: 2026-09-21
Deciders: GoBridge core
Relates to: 0012 (no-op reloads stay accepted), 0013 (the rollout candidate and
committed-artifact digests are taken over the same form)

## Context

Before the bridge replaces its runtime with a new configuration it checks
whether the new configuration is the same as the running one, and skips the
replacement when it is. Every accepted change replaces the whole runtime, so
every session disconnects and connects again. The check therefore decides
whether an update costs an outage.

That check compared the exact bytes of the document. Three places made the
decision — the Supervisor's no-op check, the configuration manager's
desired-versus-running fingerprint and the AWS runtime's skip check — each with
its own projection of the document, and each counted the following as a change:

- the same document stored again under a raised version number (every DynamoDB
  write raises it, including a rollback that restores the content already
  running);
- the same sessions, receivers, senders, bindings or routes listed in another
  order, although every other part of the document refers to them by id;
- a default left out in one document and written out in the other, such as
  `bridge.drain_timeout: 30s`;
- one duration written as `30s` and the other as `30000ms`.

Tools that generate configuration rarely write the same bytes twice, and a
shared configuration has several writers. Each of these harmless writes
reconnected every session of every owner.

The same projection is also the identity a coordinated cluster rollout agrees
on: the proposer records a digest of the candidate, every member recomputes it
over the document its own source delivered, and the durable committed-config
artifact carries the digest of what the cohort committed. That identity had to
stay deterministic across members.

## Decision

**One normal form, defined once, in `ports`.** `ports.ContentNormalForm`
returns a copy of a `BridgeConfig` in the shape every content-identity check
compares. It is a pure transformation of the blueprint, with no JSON or YAML
involved, so it can live in the inner ring next to the type it normalises. It
never modifies its input and never rewrites a plugin's decoded options.

The normal form:

- zeroes `Version`. It is the counter writers use to avoid overwriting each
  other, not something the bridge runs. Ordering of updates is still enforced
  by the callers that read it (the AWS runtime's stale-source guard runs before
  the content check), so leaving it out does not weaken that ordering;
- sorts `Sessions`, `Receivers`, `Senders`, `Bindings` and `Routes` by id,
  stably, because every other part of the document refers to their entries by
  id and their position carries no meaning;
- keeps every other list in its written order, because there the order is the
  meaning: a route's bindings (the first one is the primary session), its
  processor chain, a resolver's rules, a receiver's subscriptions, the cluster
  roster, and anything inside a plugin's own options;
- writes every duration field in `time.Duration`'s own spelling, so `30000ms`
  and `30s` are one value. A value that cannot be parsed is kept as written, so
  it still takes part in the comparison and a document that cannot be brought
  into the normal form counts as a change (fail safe);
- writes out the two defaults `ports` itself defines, `bridge.shutdown_timeout`
  and `bridge.drain_timeout`, for a value that is left out (the 30 seconds their
  accessors fall back to). A written value is kept as written, an explicit zero
  included: the validator rejects a zero timeout, and equating it with the
  default would let an invalid document be adopted as a no-op before validation
  sees it. Defaults owned by other layers (the outbox drainer, the session
  runtime, a transport) are not filled in; an unset value stays unset and
  compares as unset.

**Every content identity is taken over it.** `bridge.configContentEqual`, the
rollout candidate and committed-artifact digest (`bridge.ConfigArtifactDigest`),
the configuration manager's fingerprint and the AWS runtime's skip check (which
now calls `bridge.ConfigArtifactDigest`) all project the normal form. They can
no longer disagree about what counts as a change. Their JSON encoders stay
where they were, because the inner ring deliberately carries no JSON
dependency, but the second rule they apply to the decoded tree is shared as
well: `ports.WithoutEmptyCollections` makes an absent collection and an empty
one the same value (a document read back from a store turns a nil slice into
an empty one), in the bridge's projection and in the manager's fingerprint
alike, for the blueprint and for every plugin's options. A keyword field that
happens to look like a duration, such as `ack_after`, is compared as written.

`bridge.DeploymentBaselineContentDigest` is now the same value as
`bridge.ConfigArtifactDigest`. The name is kept because the AWS deployment
stamps it (`dynamodb_ha_baseline_config_digest`).

**A no-op reload adopts the document.** When the new configuration has the
running configuration's normal form, the runtime is kept and every session stays
connected, but the new document becomes the applied configuration: the
Supervisor's `Config()` and the AWS runtime's applied configuration report the
version number that now describes the running content, in step with the
configuration manager's running version.

**Digests recorded before the normal form are still readable.** A rollout row
and a committed-config artifact written by an earlier release carry a digest of
the raw document. When the bridge reads such a record it accepts either the
normal-form digest or the digest of the raw document, so an upgraded member
neither refuses to start on its committed configuration nor reports a false
rollout divergence. Three details make that hold after the upgrade, not only at
the moment of it:

- the raw-document digest is compared at the version the record names (both
  records carry it), so a later re-save that only raised the version still
  matches;
- once a record has matched through the raw-document digest, the bridge
  remembers, for the life of the process, which normal-form identity that
  digest stands for, so a later no-op that reorders the document still matches;
- at boot, once the committed artifact has been decoded and its integrity
  verified, the member compares content, not digests, and boots on its own
  document when that document is the committed content in another form.

The raw-document digest is never written again; the first commit on the new
release replaces the record. The fallback can be removed once no cohort can
still hold a record written before the normal form.

**The committed artifact's version is still checked, separately.** The digest
leaves the version out, so on its own it no longer proves that a decoded
artifact carries the version its record names. The writer therefore stamps the
bytes with the rollout row's version before encoding them: members whose
sources delivered equivalent documents at different versions join one rollout,
and whichever of them writes the artifact, the record and its bytes agree. A
decoded document whose version differs from the record's `ConfigVersion` is
consequently corrupt and is rejected, in boot resolution and in reconciliation
alike. That keeps the corruption check the byte digest used to give, without
putting the version back into the identity.

## Consequences

- An update that changes nothing about what the bridge does reconnects
  nothing: a rollback that restores the running content, a re-save by another
  writer, or a generator that reorders lists or writes defaults out.
- A configuration that cannot be normalised or canonicalised still counts as a
  change, as before. Nothing is skipped on a doubt.
- A version-only difference is no longer a live-safe delta a coordinated
  cohort rolls out; it is a no-op on every member. A change the barrier does
  carry keeps its exact identity across members, because the normal form is a
  deterministic function of the document.
- A cohort running mixed releases across this change should not roll out a
  configuration change until every member runs the new release: a candidate
  proposed under the raw-document digest is never staged under the normal-form
  digest, so such a rollout aborts at its deadline (it never commits a
  configuration nobody agreed on; it just does not go through).
- Defaults owned by other layers are still compared as written. A document
  that writes out, say, a route's default delivery mode still differs from one
  that leaves it out. Folding such a default in is a one-line addition to the
  normal form once its owner exposes it.

## Alternatives considered

- **A JSON projection in `ports` shared by all three checks.** Rejected: the
  inner ring carries no JSON dependency by design, and the shape is what had to
  be shared, not the encoder.
- **Keeping the exact-bytes digest for rollout candidates and committed
  artifacts and using the normal form only for the no-op checks.** Rejected:
  a member compares the digest of what it is running with the digest the cohort
  agreed on to report whether it has applied a generation. After a rollback
  restored the running content under a new version, every healthy member would
  have reported itself as diverged. One identity avoids that class of false
  alarm.
- **Filling in every default the runtime applies.** Rejected for now: those
  defaults are owned by several layers, and duplicating them in `ports` would
  let the identity drift from the runtime. The normal form fills in only the
  defaults `ports` owns.
