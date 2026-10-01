package artemislocal

import "testing"

// DefaultImage is the pinned broker image, exported to the external test
// package so a client container runs the same Artemis release as the broker.
const DefaultImage = defaultImage

// ResetOptions clears every option Configure applied, so a test configures the
// next ForceStart from scratch whatever an earlier test set. The options in
// force before the call come back when the test ends.
func ResetOptions(t testing.TB) {
	t.Helper()
	mu.Lock()
	saved := opts
	opts = options{}
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		opts = saved
		mu.Unlock()
	})
}
