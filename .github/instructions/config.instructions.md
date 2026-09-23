---
applyTo: "config/**,validate/**,ports/blueprint*.go,ports/plugin_config*.go,ports/content_tree*.go,bridge/config_content_identity*.go,bridge/builder_resolve.go"
---

# Configuration: loading, normal form and validation

Sources: ADR-0013, ADR-0016, PLUGIN.md, `docs/internals/ddd-aggregates.md`
and `docs/cluster/spec/cluster-config-rollout-protocol.md`.

## Content identity (ADR-0016)

- `ports.ContentNormalForm` is the one definition of "same config". It is
  pure: it never mutates its input or plugin options. A second equality check
  elsewhere will drift from it.
- The normal form must treat a value exactly as the runtime does, or a real
  change looks like a no-op (or a no-op reconnects every session):
  - JSON decoding uses `Decoder.UseNumber`, so an integer is never rounded
    through `float64`;
  - numbers compare the way `runtime.Val` does (`-0.0` equals `0.0`);
  - durations normalise to `time.Duration` spelling, and an unparsable value
    is kept as written so validation still fails on it.
- It sorts `Sessions`, `Receivers`, `Senders`, `Bindings`, `Routes` and
  `bridge.cluster.members` by ID, and keeps written order everywhere else:
  route bindings (the first is primary), the processor chain, resolver rules,
  subscriptions, plugin options. `Version` is zeroed.
- A new config field that changes behaviour is part of the normal form.

## Lists that must agree

- A new optional plugin capability goes into **both** freeze guards:
  `freezePluginConfig` (`bridge/builder_resolve.go`) and `freezeInitialPlugin`
  (`config/initial_snapshot.go`). A capability dropped by one is silent: the
  config still validates, and the check the capability exists for is skipped
  (PLUGIN.md).
- A new blueprint field is added to `spec/httpapi/config-components.yaml` in
  the same PR, and to the config manager's projection as well as the bridge's.

## Validation fails closed

- Invalid, negative, foreign-typed, typed-nil or trailing input is rejected.
  It is never read as "option omitted" and replaced by a default.
- `shutdown_timeout` and `drain_timeout` are filled in (30s) only when absent.
  An explicit zero is kept, and the validator rejects it.
- Every value a doc shows as valid is accepted by the parser. For example,
  durations take a unit: a bare `0` is rejected, so a doc must not say "set it
  to 0 to disable".
- Plugin config is a typed `ports.PluginConfig` with a real `Validate()`.
- `rollout: coordinated` requires `members`, and `member_id` must appear in
  `members`. `members` is not `cluster.endpoints`.
