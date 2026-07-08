package config

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestManager_ConcurrentStop_NoDoubleClose is the regression for the
// concurrent-Stop double close on the manager: running is only cleared after
// the watch loop exits, so two simultaneous Stop calls both passed the running
// guard and both closed stopCh — a second close of a channel panics. The
// stopping guard serialises the close while every caller still waits on doneCh.
// Runs under -race; a regression panics the test.
func TestManager_ConcurrentStop_NoDoubleClose(t *testing.T) {
	w := &reWatcher{}
	mgr := NewManager(
		Layer{Name: "file", Loader: &stubLoader{cfg: minimalValidConfig("bridge1")}, Watcher: w},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := mgr.Watch(ctx)
	require.NoError(t, err)

	const stoppers = 8
	var wg sync.WaitGroup
	wg.Add(stoppers)
	start := make(chan struct{})
	for i := 0; i < stoppers; i++ {
		go func() {
			defer wg.Done()
			<-start // release all at once to widen the race window
			mgr.Stop()
		}()
	}
	close(start)
	wg.Wait() // a double close would have panicked before we reach here

	// A subsequent Stop is a no-op, and Watch restarts cleanly because it
	// re-arms the stopping guard.
	mgr.Stop()
	_, err = mgr.Watch(ctx)
	require.NoError(t, err)
	mgr.Stop()
}
