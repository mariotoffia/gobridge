# 0001 — Reserved-header trust model and out-of-band signaling

Status: accepted
Date: 2026-07-03
Deciders: GoBridge core

## Context

Bridge internals need to steer routing per message: a DLQ redrive must replay a
message to the one binding that failed, not fan it out to every binding on the
route. The obvious channel is a header — set `x-bridge.route-override` on the
message and let the route runner read it.

That channel is unsafe. A message arriving from a transport is external input.
If routing honored a header on inbound traffic, any producer could set
`x-bridge.route-override` and steer its message to an arbitrary binding,
bypassing the route's filters and destinations. Headers cross the trust
boundary; routing decisions must not depend on them.

All bridge-internal headers share the reserved prefix `x-bridge.`
(`domain/messaging/headers.go:8`), including `HeaderRouteOverride =
"x-bridge.route-override"` (`headers.go:28`).

## Decision

Strip every reserved header at pipeline ingress, then re-stamp trusted signals
out-of-band from typed struct fields — never from the wire.

- **Strip at ingress.** `doHandleDelivery` calls
  `StripReservedHeaders` on the envelope before any consumption site reads it
  (`runtime/route/runner.go:383`,
  `env.ReplaceHeaders(messaging.StripReservedHeaders(env.Headers()))`). This is
  the sole chokepoint. `StripReservedHeaders` drops every key matching the
  reserved prefix (`headers.go:93`). An externally-supplied
  `x-bridge.route-override` is gone before routing looks at it.

- **Re-stamp from a typed field.** A trusted binding override rides a Go struct
  field, not a header. `Runtime.InjectToBinding` constructs a
  `syntheticDelivery{env, binding}` where the binding ID is a struct field
  (`runtime/bridge_routes.go:131`, `:161-172`). Only an internally-constructed
  delivery satisfies the `bindingOverrider` interface
  (`runtime/route/runner.go:335`); transport-created deliveries never implement
  it. After the strip, `doHandleDelivery` re-stamps `HeaderRouteOverride` from
  that interface (`runner.go:396-400`) so the override is present for the
  routing decision but originated in-process, not from the message.

- **Processors may set overrides post-strip.** A processor running after ingress
  is trusted code and may legitimately set a route override. The strip runs
  before the processor chain, so a processor-set override survives.

## Consequences

- External messages can never steer routing. A spoofed `x-bridge.*` header is
  discarded at ingress regardless of source.
- The binding-scoped DLQ redrive (ADR 0006, retained by ADR 0015) works because the binding travels on
  a typed field through `InjectToBinding`, immune to the strip.
- Any future trusted in-process signal must follow the same rule: carry it on a
  typed struct field or interface, re-applied after the ingress strip. Adding a
  new `x-bridge.*` header and expecting routing to honor it on inbound traffic
  is a security regression — the strip will erase it, and it should.
- There is one strip site to audit. New consumption paths must sit downstream of
  `doHandleDelivery`, or add their own strip.

## Rejected alternatives

- **Trust `x-bridge.*` headers on a subset of sources.** Requires classifying
  every delivery as trusted or untrusted at every consumption site, and one
  missed classification is an authorization bypass. A single unconditional strip
  is simpler and fails safe.
- **Encrypt or sign internal headers.** Adds key management and per-message
  crypto to solve a problem that does not exist once signals travel in-process
  on typed fields. The header channel is the liability; removing it is cheaper
  than securing it.
