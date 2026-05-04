package runtime

// AttachCredentialCloser registers a close-on-stop hook with the runtime.
// The runtime invokes this closure during Stop, before session teardown,
// so any goroutines that call ApplyCredentials on a session can be
// cancelled safely.
//
// Passing a closure (rather than an interface value) deliberately keeps
// runtime free of any structural reference to a caller-defined type:
// the runtime sees only func(); deep architecture analysis cannot infer
// a phantom dependency on the caller's package.
func AttachCredentialCloser(rt *Runtime, close func()) {
	if rt == nil || close == nil {
		return
	}
	rt.mu.Lock()
	rt.credRefresherClose = close
	rt.mu.Unlock()
}
