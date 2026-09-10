package bridge

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// syncBuffer is a mutex-guarded io.Writer so a slog handler written from the
// refresher's watch goroutine can be read by the test goroutine without a data
// race under -race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// reactivePushStore is an inlinePushStore that ALSO implements the reactive
// refresh capability (Refresh(uri)), recording every Refresh call so a test can
// prove the auth-failure hook forwards to it.
type reactivePushStore struct {
	out          chan *connectivity.CredentialSet
	refreshCalls chan string
}

func (p *reactivePushStore) Watch(ctx context.Context, _ string) (<-chan *connectivity.CredentialSet, error) {
	proxy := make(chan *connectivity.CredentialSet, 1)
	go func() {
		defer close(proxy)
		for {
			select {
			case <-ctx.Done():
				return
			case c, ok := <-p.out:
				if !ok {
					return
				}
				select {
				case proxy <- c:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return proxy, nil
}

func (p *reactivePushStore) Refresh(uri string) {
	select {
	case p.refreshCalls <- uri:
	default:
	}
}

// authRejectingSession rejects every ApplyCredentials with ErrNotAuthorized so
// a test can exercise the apply-error reactive-refresh path.
type authRejectingSession struct {
	*fakeSession
}

func (s *authRejectingSession) ApplyCredentials(_ context.Context, _ *connectivity.CredentialSet) error {
	return shared.ErrNotAuthorized
}

// blockingSession blocks ApplyCredentials until release is closed (it ignores
// ctx), simulating a wedged transport so a test can prove the fan-out is
// concurrent — other targets must still be applied.
type blockingSession struct {
	*fakeSession
	release chan struct{}
}

func (s *blockingSession) ApplyCredentials(_ context.Context, _ *connectivity.CredentialSet) error {
	<-s.release
	return nil
}

// ctxHonoringSession blocks ApplyCredentials until ITS context is cancelled,
// then reports the ctx error. Used to prove the per-apply timeout fires.
type ctxHonoringSession struct {
	*fakeSession
	returned chan error
}

func (s *ctxHonoringSession) ApplyCredentials(ctx context.Context, _ *connectivity.CredentialSet) error {
	<-ctx.Done()
	err := ctx.Err()
	select {
	case s.returned <- err:
	default:
	}
	return err
}

// TestCredentialRefresher_NotifyAuthFailureForcesRefresh verifies the hook:
// a NOT_AUTHORIZED report on a watched URI forwards to the push store's
// reactive Refresh, while a non-authorization error does not.
func TestCredentialRefresher_NotifyAuthFailureForcesRefresh(t *testing.T) {
	t.Parallel()

	push := &reactivePushStore{
		out:          make(chan *connectivity.CredentialSet, 1),
		refreshCalls: make(chan string, 4),
	}
	r := NewCredentialRefresher(push, nil)
	defer r.Close()

	r.NotifyAuthFailure("file://creds", shared.ErrNotAuthorized)
	require.Equal(t, "file://creds", wait.RequireReceive(t, push.refreshCalls, 2*time.Second),
		"NOT_AUTHORIZED must force a reactive re-resolve")

	// A non-authorization error must NOT trigger a reactive refresh.
	r.NotifyAuthFailure("file://creds", shared.ErrUnavailable)
	wait.Silent(t, push.refreshCalls, 100*time.Millisecond)
}

// TestCredentialRefresher_ApplyAuthFailureTriggersRefresh verifies the
// apply-error path: when a transport rejects rotated credentials with
// NOT_AUTHORIZED, the refresher forces an out-of-band re-resolve.
func TestCredentialRefresher_ApplyAuthFailureTriggersRefresh(t *testing.T) {
	t.Parallel()

	push := &reactivePushStore{
		out:          make(chan *connectivity.CredentialSet, 1),
		refreshCalls: make(chan string, 4),
	}
	r := NewCredentialRefresher(push, nil)
	defer r.Close()

	r.Watch(t.Context(), "file://creds", &authRejectingSession{fakeSession: &fakeSession{}})
	push.out <- connectivity.NewCredentialSet(pwCred("u", "p"), nil)

	require.Equal(t, "file://creds", wait.RequireReceive(t, push.refreshCalls, 2*time.Second),
		"an ApplyCredentials NOT_AUTHORIZED must force a reactive re-resolve")
}

// reportingSession is a CredentialAware target that ALSO implements
// AuthFailureReporter. It captures the URI-bound callback the
// refresher injects at Watch time so a test can drive it directly and prove it
// forwards to the reactive push store's Refresh.
type reportingSession struct {
	*fakeSession
	mu sync.Mutex
	cb func(error)
}

func (s *reportingSession) ApplyCredentials(_ context.Context, _ *connectivity.CredentialSet) error {
	return nil
}

func (s *reportingSession) SetAuthFailureCallback(cb func(error)) {
	s.mu.Lock()
	s.cb = cb
	s.mu.Unlock()
}

func (s *reportingSession) callback() func(error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cb
}

// TestCredentialRefresher_InjectsURIBoundAuthFailureCallback verifies the
// injection: a target implementing AuthFailureReporter receives a
// URI-bound callback at Watch time that, when invoked with
// shared.ErrNotAuthorized, forces the reactive Refresh for that URI; a non-auth
// error does not.
func TestCredentialRefresher_InjectsURIBoundAuthFailureCallback(t *testing.T) {
	t.Parallel()

	push := &reactivePushStore{
		out:          make(chan *connectivity.CredentialSet, 1),
		refreshCalls: make(chan string, 4),
	}
	r := NewCredentialRefresher(push, nil)
	defer r.Close()

	sess := &reportingSession{fakeSession: &fakeSession{}}
	r.Watch(t.Context(), "file://creds", sess)

	cb := sess.callback()
	require.NotNil(t, cb, "refresher must inject a URI-bound auth-failure callback")

	// An authorization failure forces a reactive re-resolve bound to the URI.
	cb(shared.ErrNotAuthorized)
	require.Equal(t, "file://creds", wait.RequireReceive(t, push.refreshCalls, 2*time.Second),
		"a live NOT_AUTHORIZED report must force a reactive re-resolve for the bound URI")

	// A non-authorization failure must NOT trigger a reactive refresh.
	cb(shared.ErrUnavailable)
	wait.Silent(t, push.refreshCalls, 100*time.Millisecond)
}

// TestCredentialRefresher_NonReporterTargetSkippedCleanly verifies that a
// CredentialAware target WITHOUT the AuthFailureReporter capability is watched
// normally (rotation still applies) and is not required to expose the setter —
// the injection is silently skipped, mirroring the CredentialAware tolerance.
func TestCredentialRefresher_NonReporterTargetSkippedCleanly(t *testing.T) {
	t.Parallel()

	push := &reactivePushStore{
		out:          make(chan *connectivity.CredentialSet, 1),
		refreshCalls: make(chan string, 4),
	}
	r := NewCredentialRefresher(push, nil)
	defer r.Close()

	// fakeSession is CredentialAware-shaped via authRejectingSession but does
	// NOT implement AuthFailureReporter; Watch must not panic and rotation
	// still drives ApplyCredentials (which here rejects and self-reports).
	require.NotPanics(t, func() {
		r.Watch(t.Context(), "file://creds", &authRejectingSession{fakeSession: &fakeSession{}})
	})
	push.out <- connectivity.NewCredentialSet(pwCred("u", "p"), nil)

	// The apply-error path still forces a reactive refresh (existing), proving
	// the non-reporter target is watched cleanly.
	require.Equal(t, "file://creds", wait.RequireReceive(t, push.refreshCalls, 2*time.Second))
}

// TestCredentialRefresher_ApplyErrorNeverLeaksURIUserinfo is the refresher
// leak-regression test: a watched URI that embeds userinfo (user:pass@) must
// never appear verbatim in any log line, even on the ApplyCredentials-failure
// path that logs the URI. The refresher redacts every uri log field
// (shared.RedactURI), so the embedded secret is stripped before it is logged.
func TestCredentialRefresher_ApplyErrorNeverLeaksURIUserinfo(t *testing.T) {
	t.Parallel()

	const secret = "s3cr3t"
	uri := "file://user:" + secret + "@creds"

	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	push := &reactivePushStore{
		out:          make(chan *connectivity.CredentialSet, 1),
		refreshCalls: make(chan string, 4),
	}
	r := NewCredentialRefresher(push, logger)

	r.Watch(t.Context(), uri, &authRejectingSession{fakeSession: &fakeSession{}})
	push.out <- connectivity.NewCredentialSet(pwCred("u", "p"), nil)

	// The apply-error path (WARN "ApplyCredentials failed") and the reactive
	// hook (DEBUG "auth failure -> forcing reactive re-resolve") both log the
	// URI; wait until the reactive Refresh fires so both lines are written.
	wait.RequireReceive(t, push.refreshCalls, 2*time.Second)

	// Close joins the watch goroutine so no further log writes race the read.
	r.Close()

	require.NotContains(t, logs.String(), secret,
		"no refresher log line may contain the URI userinfo secret")
}

// TestCredentialRefresher_SlowApplyDoesNotBlockOtherTargets proves the rotation
// fan-out is CONCURRENT: a target whose ApplyCredentials is wedged must not
// delay the rotation reaching the other targets sharing the URI.
func TestCredentialRefresher_SlowApplyDoesNotBlockOtherTargets(t *testing.T) {
	t.Parallel()

	push := &countingPushStore{out: make(chan *connectivity.CredentialSet, 1)}
	r := NewCredentialRefresher(push, nil)

	release := make(chan struct{})
	// Deferred LIFO: release the wedged apply FIRST, then Close waits cleanly.
	defer r.Close()
	defer close(release)

	slow := &blockingSession{fakeSession: &fakeSession{}, release: release}
	fast := &credAwareFakeSession{
		fakeSession: &fakeSession{},
		applied:     make(chan *connectivity.CredentialSet, 1),
	}

	// Same URI -> ONE poller, fan-out to both targets.
	r.Watch(t.Context(), "file://creds", slow)
	r.Watch(t.Context(), "file://creds", fast)

	push.out <- connectivity.NewCredentialSet(pwCred("u2", "p2"), nil)

	got := wait.RequireReceive(t, fast.applied, 2*time.Second)
	require.Equal(t, "u2", got.Password().Username(),
		"a wedged target must not block the rotation reaching a healthy target")
}

// TestCredentialRefresher_PerApplyTimeoutBoundsHungApply proves each
// ApplyCredentials runs under a bounded context: a ctx-honouring but hung apply
// is cancelled after the configured WithApplyTimeout.
func TestCredentialRefresher_PerApplyTimeoutBoundsHungApply(t *testing.T) {
	t.Parallel()

	push := &inlinePushStore{out: make(chan *connectivity.CredentialSet, 1)}
	r := NewCredentialRefresher(push, nil, WithApplyTimeout(50*time.Millisecond))
	defer r.Close()

	sess := &ctxHonoringSession{fakeSession: &fakeSession{}, returned: make(chan error, 1)}
	r.Watch(t.Context(), "file://creds", sess)

	push.out <- connectivity.NewCredentialSet(pwCred("u", "p"), nil)

	err := wait.RequireReceive(t, sess.returned, 2*time.Second)
	require.Error(t, err, "a hung ApplyCredentials must be cancelled by the per-apply timeout")
}

// TestCredentialRefresher_EmitsRotationAppliedMetric verifies the success
// counter: MetricCredentialRotationApplied is emitted once per applied rotation.
func TestCredentialRefresher_EmitsRotationAppliedMetric(t *testing.T) {
	t.Parallel()

	push := &inlinePushStore{out: make(chan *connectivity.CredentialSet, 1)}
	rec := &ports.RecordingExporter{}
	r := NewCredentialRefresher(push, nil, WithRefresherMetrics(rec))
	defer r.Close()

	sess := &credAwareFakeSession{
		fakeSession: &fakeSession{},
		applied:     make(chan *connectivity.CredentialSet, 2),
	}
	r.Watch(t.Context(), "file://creds", sess)

	push.out <- connectivity.NewCredentialSet(pwCred("u2", "p2"), nil)
	wait.RequireReceive(t, sess.applied, 2*time.Second)

	wait.Until(t, 2*time.Second, "rotation-applied metric emitted", func() bool {
		return len(rec.FindEntries(shared.MetricCredentialRotationApplied)) >= 1
	})
}
