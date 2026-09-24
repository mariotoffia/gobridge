package runtime

import (
	"sync"
	"time"
)

// autoRedriveState is the runtime's automatic-redrive configuration and the
// lock that runs one redrive pass at a time (ADR 0019).
type autoRedriveState struct {
	window time.Duration
	mu     sync.Mutex //nolint:unused // the redrive pass lock; drop this directive once the pass takes it
}

// WithAutoRedriveWindow sets how old a dead-letter record may be and still be
// redriven by a matching system event by itself (ADR 0019). Zero or a negative
// value turns automatic redrive off. Without the option the window is
// ports.DefaultAutoRedriveWindow.
func WithAutoRedriveWindow(d time.Duration) Option {
	return func(rt *Runtime) { rt.autoRedrive.window = max(d, 0) }
}
