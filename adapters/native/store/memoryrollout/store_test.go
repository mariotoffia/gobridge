package memoryrollout_test

import (
	"io"
	"log/slog"
	"testing"

	"github.com/mariotoffia/gobridge/adapters/native/store/memoryrollout"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/ports/storetest"
)

// Compile-time assertion that *Store satisfies the port it adapts. It lives in
// the external test package because the store adapter layer may not depend on
// ports (the production package satisfies ports.ClusterRolloutStore
// structurally).
var _ ports.ClusterRolloutStore = (*memoryrollout.Store)(nil)

func newTestStore() *memoryrollout.Store {
	return memoryrollout.NewStore(
		memoryrollout.WithClock(clocktest.New()),
		memoryrollout.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
}

// TestConformanceSuite validates the in-memory rollout store against the shared
// conformance suite. Each subtest gets a fresh single-point store.
func TestConformanceSuite(t *testing.T) {
	storetest.RunClusterRolloutStoreTests(t, func() ports.ClusterRolloutStore {
		return newTestStore()
	})
}
