// Package cluster owns the runtime's cluster-coordination primitives:
// the route-ownership [Locator] that combines lease state with the
// route → session mapping to decide whether this instance handles a
// given route or forwards to the peer that holds the lease, the peer
// endpoint projection, and the failure-cooldown circuit that keeps a
// flaky lease store from amplifying into per-message latency.
//
// This package is a leaf within the runtime layer. It depends only on
// inward layers (`domain/*`, `ports`). The parent `runtime` package
// composes a *cluster.Locator and exposes it through the
// [ports.RouteLocator] interface; the dependency direction is
// parent -> leaf only. This leaf MUST NOT depend on its parent
// (`runtime`), on any sibling runtime leaf, on any adapter, on bridge,
// or on the composition root. Treat any new outward edge here as a
// smell that the leaf is absorbing orchestration concerns it should
// publish back to runtime instead.
package cluster
