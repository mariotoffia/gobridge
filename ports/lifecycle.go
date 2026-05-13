package ports

import "context"

// RouteIDSetter is implemented by transport adapters (Receiver, Sender,
// Session) that wish to be informed of the route ID they are wired into.
//
// The runtime probes this capability via a type assertion when starting a
// route and, when present, calls SetRouteID with the static route
// identifier. Adapters typically use the route ID as a label on metrics,
// log fields, and trace attributes so per-route signals can be correlated
// without the runtime having to thread route context through every call.
//
// Implementations MUST be safe to invoke once during route start; the
// runtime does not call SetRouteID concurrently or after start completes.
type RouteIDSetter interface {
	SetRouteID(routeID string)
}

// ContextCloser is the optional context-aware closer interface implemented
// by transport adapters and other long-lived components. It mirrors
// io.Closer but accepts a deadline-bearing context so the runtime can cap
// teardown latency during Stop.
//
// The runtime probes this capability via a type assertion during route /
// session shutdown. Adapters that implement it SHOULD honour ctx.Done and
// return promptly when the context is cancelled; resource cleanup that
// outlives ctx must be detached on a background goroutine.
type ContextCloser interface {
	Close(ctx context.Context) error
}
