package servicebus

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// --- c6-dead-link (fix #1): rebuild commit is fenced against a rotation ------

// TestReceiver_NonSessionRebuild_RotationWinsStaleRebuildDiscarded pins the
// generation fence on the non-session dead-link rebuild path. The exact
// interleaving the re-review flagged:
//
//	cold init builds stack0 (conn=rotCS1)
//	  → poll loop starts rebuildReceiver (captures the current generation,
//	    then BLOCKS building a fresh rotCS1 stack)
//	  → ApplyCredentials(rotCS2) commits stack1 on a NEW connection
//	    (commitStack bumps rebuildGen, closes stack0)
//	  → unblock the rebuild: its commit must be FENCED (generation moved),
//	    so the freshly built stale-connection stack is DISCARDED (closed)
//	    and cfg.Connection stays on the rotation's rotCS2.
//
// This lost update is invisible to -race (every field is initMu-guarded);
// only the fence prevents the rebuild from reverting cfg.Connection to the
// revoked rotCS1 and closing the rotation's live stack.
//
// Mutation: revert rebuildReceiver to an unconditional commitStack (drop
// the commitRebuild generation fence). Then the stale rebuild overwrites
// the live stack with its rotCS1 build and rolls cfg.Connection back to
// rotCS1 → the "rotation wins" assertions FAIL.
func TestReceiver_NonSessionRebuild_RotationWinsStaleRebuildDiscarded(t *testing.T) {
	t.Parallel()

	var (
		mu           sync.Mutex
		coldStack    *closeableASBClient
		rotStack     *closeableASBClient
		rebuildStack *closeableASBClient
		cs1Builds    int
		gate         chan struct{} // blocks the in-flight rebuild build of rotCS1
		began        chan struct{} // closed once the gated rebuild build begins
	)

	build := func(_ context.Context, conn ConnectionConfig) (receiverStack, error) {
		key := conn.ConnectionString.Reveal()
		mu.Lock()
		if key == rotCS1 {
			cs1Builds++
			if cs1Builds == 1 {
				// Cold init: return immediately.
				c := &closeableASBClient{}
				coldStack = c
				mu.Unlock()
				return receiverStack{client: c}, nil
			}
			// The poll loop's dead-link rebuild of rotCS1: signal that it
			// has captured the generation and begun, then block on the gate
			// so the rotation can land while this build is in flight.
			g, b := gate, began
			mu.Unlock()
			if b != nil {
				close(b)
			}
			if g != nil {
				<-g
			}
			c := &closeableASBClient{}
			mu.Lock()
			rebuildStack = c
			mu.Unlock()
			return receiverStack{client: c}, nil
		}
		// rotCS2 — the rotation's fresh stack.
		c := &closeableASBClient{}
		rotStack = c
		mu.Unlock()
		return receiverStack{client: c}, nil
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "q",
		Connection: ConnectionConfig{ConnectionString: shared.NewSecret(rotCS1)},
	}, nil)
	require.NoError(t, err)
	recv.buildStackFn = build

	ctx := context.Background()
	require.NoError(t, recv.ensureClient(ctx)) // builds coldStack (rotCS1)
	mu.Lock()
	require.Same(t, coldStack, recv.currentClient())
	mu.Unlock()

	// Arm the gate for the in-flight rebuild.
	mu.Lock()
	gate = make(chan struct{})
	began = make(chan struct{})
	g, b := gate, began
	mu.Unlock()

	// Poll loop's dead-link rebuild of rotCS1: captures the generation,
	// then blocks in build.
	rebuildDone := make(chan error, 1)
	go func() { rebuildDone <- recv.rebuildReceiver(ctx) }()
	<-b // rebuild captured the generation and is blocked on the gate

	// Rotation to rotCS2 lands successfully while the rebuild is stuck:
	// commitStack installs rotStack, bumps rebuildGen, closes coldStack.
	set := connectivity.NewCredentialSet(pwCred("", rotCS2), nil)
	require.NoError(t, recv.ApplyCredentials(ctx, set))
	require.Same(t, rotStack, recv.currentClient(), "rotation's fresh stack is live")
	require.Equal(t, rotCS2, connSnapshot(recv).ConnectionString.Reveal())

	// Unblock the stale rebuild: its commit must be fenced (generation
	// moved), discarding its rotCS1 stack and leaving rotCS2 intact.
	close(g)
	require.NoError(t, <-rebuildDone)

	require.Same(t, rotStack, recv.currentClient(),
		"the stale rebuild must NOT clobber the rotation's stack")
	require.Equal(t, rotCS2, connSnapshot(recv).ConnectionString.Reveal(),
		"cfg.Connection must stay on the rotation's rotCS2, not revert to the revoked rotCS1")
	require.Equal(t, int32(1), rebuildStack.closeCalls.Load(),
		"the superseded rebuild stack is discarded (closed)")
	require.Equal(t, int32(0), rotStack.closeCalls.Load(),
		"the live rotation stack is not closed")
	require.Equal(t, int32(1), coldStack.closeCalls.Load(),
		"the cold stack was closed exactly once by the rotation")
}
