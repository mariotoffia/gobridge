// Package runtime implements the GoBridge route execution engine.
//
// It orchestrates message flow through routes using the port interfaces
// defined in ports/ and the value types from domain/. The runtime has
// no transport or storage dependencies -- those are injected via port
// interfaces at construction time.
//
// The runtime is split into six leaf sub-packages, each owning a
// single concern:
//
//   - runtime/dlq:         dead-letter-queue router (classify → enqueue → drain)
//   - runtime/cluster:     route-ownership locator + peer endpoint projection
//   - runtime/session:     lease lifecycle, three-phase step-down, reconnect
//   - runtime/outbox:      shared-outbox Drainer, DepthCache, timeout scaling
//   - runtime/route:       per-route ingress pipeline, dispatch, retry/backoff
//   - runtime/credentials: PullCredentialStore → PushCredentialStore wrapper
//     (consumed by bridge composition root, not runtime)
//
// The dependency direction is parent → leaf only. Leaves never import
// their parent nor unrelated siblings; see .go-arch-lint.yml for the
// enforced edges.
package runtime
