package runtime

// credentialRefresher is a narrow closer interface used by the runtime
// to tear down credential watcher goroutines when Stop() runs. The
// concrete implementation lives in the bridge package
// (bridge.CredentialRefresher); keeping the interface here avoids a
// runtime → bridge import cycle.
type credentialRefresher interface {
	Close()
}

// AttachCredentialRefresher registers a credential refresher with the
// runtime so that Stop() closes it alongside sessions. This is
// package-level rather than a method to match the existing option /
// attach style (runtime deliberately avoids exporting state mutators
// on *Runtime; concrete wiring goes through these helpers).
//
// Why: the bridge package owns the watcher lifecycle, but its goroutines
// must be cancelled before the session is closed (otherwise a rotation
// racing with Close can call ApplyCredentials on a closing session).
// Routing the close through the runtime gives us deterministic shutdown
// ordering for free.
func AttachCredentialRefresher(rt *Runtime, r credentialRefresher) {
	if rt == nil || r == nil {
		return
	}
	rt.mu.Lock()
	rt.credRefresher = r
	rt.mu.Unlock()
}
