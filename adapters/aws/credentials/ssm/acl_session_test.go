package ssm

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// TestSession_InitFailureThenSuccessRecovers verifies that a transient SSM
// client construction failure is NOT cached permanently: a later ensure()
// call retries construction and recovers.
//
// Mutation reasoning: restoring the pre-fix ensure() that memoised the
// first init error via sync.Once/initErr makes the second call return the
// cached error instead of retrying — the require.NoError below then fails.
// This is the c12-ssm-poison regression guard: a cold/standby pod hitting
// a one-time IMDS/token/deadline blip must not stay poisoned until restart.
func TestSession_InitFailureThenSuccessRecovers(t *testing.T) {
	t.Parallel()

	wantClient := &mockSSM{}
	var calls int
	s := newSession("us-east-1", "", "", nil)
	s.build = func(context.Context) (ssmAPI, error) {
		calls++
		if calls == 1 {
			return nil, shared.ErrUnavailable.Wrap(errors.New("transient IMDS blip"))
		}
		return wantClient, nil
	}

	// First attempt fails transiently.
	_, err := s.ensure(context.Background())
	require.Error(t, err)

	// Second attempt must retry construction and recover.
	got, err := s.ensure(context.Background())
	require.NoError(t, err)
	require.Same(t, wantClient, got.(*mockSSM))
	assert.Equal(t, 2, calls, "failed init must be retried, not cached")

	// Third attempt reuses the cached client — no further construction.
	got2, err := s.ensure(context.Background())
	require.NoError(t, err)
	require.Same(t, wantClient, got2.(*mockSSM))
	assert.Equal(t, 2, calls, "successfully built client must be cached")
}

// TestSession_EnsureConcurrentSingleBuild verifies the double-checked
// locking in ensure() builds the client exactly once under concurrency and
// is race-clean (run under -race). Guards against a regression that drops
// the mutex or memoisation and rebuilds per call.
func TestSession_EnsureConcurrentSingleBuild(t *testing.T) {
	t.Parallel()

	wantClient := &mockSSM{}
	var mu sync.Mutex
	var calls int
	s := newSession("us-east-1", "", "", nil)
	s.build = func(context.Context) (ssmAPI, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return wantClient, nil
	}

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			got, err := s.ensure(context.Background())
			assert.NoError(t, err)
			if assert.NotNil(t, got) {
				assert.Same(t, wantClient, got.(*mockSSM))
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, calls, "successful client must be built exactly once")
}

// TestSession_PresetShortCircuits verifies a preset (WithClient) client
// bypasses construction entirely and never touches the build hook.
func TestSession_PresetShortCircuits(t *testing.T) {
	t.Parallel()

	preset := &mockSSM{}
	s := newSession("", "", "", preset)
	s.build = func(context.Context) (ssmAPI, error) {
		t.Fatal("build must not be called when a preset client is supplied")
		return nil, nil
	}

	got, err := s.ensure(context.Background())
	require.NoError(t, err)
	require.Same(t, preset, got.(*mockSSM))
}
