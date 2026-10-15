# 0025 — Ignore unknown fields in a configuration document the bridge stored

Status: accepted
Date: 2026-10-15
Deciders: GoBridge core
Relates to: [0013](0013-coordinated-cluster-config-rollout.md) (the
last-committed config artifact), [0016](0016-config-content-normal-form.md)
(the content digest)

## Context

### The parser refuses every field it does not know

`config/parser.Parse` reads a configuration document in two stages, and both
refuse a field they do not know:

- the first stage decodes the document into the blueprint with
  `DisallowUnknownFields` (JSON) or `KnownFields(true)` (YAML), so an unknown
  key at the top level or inside a known section fails the parse;
- the second stage hands each plugin's `options` block to the decoder the
  plugin registered, through a `ports.RawConfig` that decodes with
  `ErrorUnused`, so an unknown option fails the parse too.

This exists to catch a typo. `shutdown_timout: 30s` is refused instead of
running with the default the person did not mean.

### Two documents are written by the bridge and read back later

- **The DynamoDB configuration document.** The DynamoDB config source
  (`adapters/aws/config/dynamodb`) stores the document with
  `parser.MarshalBridgeConfigJSON` and reads it back on every `Load`, every
  watch poll or stream event, and every observation.
- **The cluster rollout's last-committed artifact** (ADR 0013). A coordinated
  cluster stores the committed document, and a member decodes it when it
  starts or catches up on a commit it missed.

The release that reads a document is not always the release that stored it.
During a rolling deploy, tasks of two releases read the same row. After a
rollback, the older release reads what the newer release wrote. A release
that adds a field writes it whenever the field is set. The older release then
refuses the whole document: the DynamoDB source reports
`dynamodb config load: parse`, the task does not start or reload, and a
joining cluster member cannot rebuild the committed configuration.

### An unknown field in a stored document is not a typo

Every document reaches storage through one of two writers:

- a strict path a person uses — a configuration file, the initial document
  built into the binary, or the admin API — which refuses a typo before the
  document is stored;
- the library's own marshaller, which writes only the fields its release
  knows.

So an unknown field in a stored document means another release wrote it.
Refusing it protects nothing and stops a bridge that could run the document.

## Decision

**A configuration document the bridge stored is read leniently: an unknown
field is ignored at every level, plugin `options` included. A document a
person writes is still read strictly.**

### 1. One parser option

`parser.Parse` takes functional options. `parser.WithIgnoreUnknownFields()`:

- skips `DisallowUnknownFields` / `KnownFields(true)` in the first stage;
- marks the `ports.RawConfig` the registry passes to each plugin decoder, so
  that decoder's `Decode` runs without `ErrorUnused`.

No plugin changes. A decoder that decodes through `RawConfig.Decode` ignores
an unknown option under the option and refuses it without.

### 2. What stays refused

The option ignores unknown fields and nothing else. These are still refused:

- **an unknown plugin kind** — a `transport` or store `type` with no
  registered decoder. It is a value outside a fixed list, not an extra field.
  The bridge has no adapter to build the component, and dropping it would run
  the bridge without a session or store the document declares;
- a known field whose value has the wrong type;
- trailing data after the document, and a document larger than
  `parser.MaxConfigBytes`;
- every check a plugin decoder's `Validate` and the configuration validator
  make.

### 3. Who reads leniently

Exactly two readers pass the option:

- the DynamoDB config source's single read path, `Loader.load`, which serves
  `Load`, `Watch` (poll and stream) and `Observe`;
- the AWS runtime's rollout codec, which decodes the last-committed artifact.

Every other reader stays strict: `parser.ParseFile`, the file config source
and its watcher, `parser.NewInlineSource` and the embedded initial document,
the admin API, and the CDK configuration source.

## Consequences

- A task reads a configuration document, or a committed artifact, that the
  next release stored. It runs the document as if the fields it does not know
  were absent, so each of them takes the default this release uses.
- A field this release ignored is not kept. When this release saves the
  document — an admin API change applied by an older task, for example —
  `parser.MarshalBridgeConfigJSON` writes only the fields it knows, so the
  newer field is dropped from storage and the newer release then reads its
  default.
- The content digest (ADR 0016) of a document read leniently covers only the
  fields this release knows. A coordinated cluster's rollout record carries the
  digest the committing member computed. When the committed document sets a
  field one member does not know, that member computes another digest, and the
  record's integrity check still refuses the record on boot and on reconcile.
  Lenient reading removes the decode failure. It does not let a cohort that
  runs two releases agree on a document that uses a field only one of them
  knows.
- A typo written by hand straight into the DynamoDB table bypasses every strict
  path, and is now ignored instead of refused. Write the document through the
  admin API or the initial document, which read it strictly.

## Rejected alternatives

- **Read every document leniently.** A typo in a file a person writes would
  run with a default nobody meant, which is what strict reading exists to
  prevent.
- **Ignore an unknown plugin kind as well.** The bridge would start without a
  component the document declares, and the routes that use it would fail or
  lose messages later instead of the load failing now.
- **Keep the stored documents strict and never roll back.** A rollback is an
  operator's way out of a bad release. Refusing the stored document turns that
  rollback into an outage.
- **A decoder for each document version.** Every release would carry the
  decoders of the releases before it, and two decoders would read one format.
- **Leave out new fields on write.** The writer cannot know which release reads
  the document next.
