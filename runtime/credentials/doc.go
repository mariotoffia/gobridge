// Package credentials owns the credential-store adaptation leaf for
// the runtime layer. Its primary export, [PollBasedWrapper], lifts a
// [ports.PullCredentialStore] into a [ports.PushCredentialStore] by
// polling on a fixed cadence and emitting a change-detected stream.
//
// Splitting this off the core runtime engine keeps route execution
// free of credential-store plumbing and lets the composition root
// import it without dragging in the runtime machinery. Only `bridge`
// consumes it — runtime itself never imports it.
//
// This package is a leaf within the runtime layer. It depends only on
// inward layers (`domain/*`, `ports`). It MUST NOT depend on its
// parent (`runtime`), on any sibling runtime leaf, on any adapter, on
// bridge, or on the composition root — the dependency direction is
// composition-root -> leaf only. Treat any new outward edge here as a
// smell that the leaf is absorbing orchestration concerns it should
// publish back to runtime instead.
package credentials
