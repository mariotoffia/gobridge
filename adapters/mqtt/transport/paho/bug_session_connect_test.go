package paho

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// fakeLiveConn is a controllable pahoConnection double that records how
// many times it was Disconnected. It stands in for a live autopaho
// ConnectionManager via Session.connectOverride so the credential-driven
// Reload (finding 1) and the Close/Start race (finding 3) can be exercised
// without a real broker.
type fakeLiveConn struct {
	disconnects *atomic.Int32
}

func (f *fakeLiveConn) AwaitConnection(context.Context) error { return nil }
func (f *fakeLiveConn) Disconnect(context.Context) error {
	if f.disconnects != nil {
		f.disconnects.Add(1)
	}
	return nil
}
func (f *fakeLiveConn) Subscribe(context.Context, []subscribeSpec) ([]byte, error) {
	return nil, nil
}
func (f *fakeLiveConn) Unsubscribe(context.Context, []string) ([]byte, error) { return nil, nil }
func (f *fakeLiveConn) PublishEnvelope(
	context.Context, *messaging.Envelope, string, SenderOptions, clock.Clock,
) (publishResult, error) {
	return publishResult{}, nil
}
func (f *fakeLiveConn) Underlying() *autopaho.ConnectionManager { return nil }

var _ pahoConnection = (*fakeLiveConn)(nil)

// ═══════════════════════════════════════════════════════════════════════════
// Finding 1 (CRITICAL): password/username-only rotation permanently kills a
// live session. The old path called cm.Disconnect, which in paho.golang
// v0.23.0 cancels the CM root context TERMINALLY (autopaho mainLoop breaks,
// never reconnects, skips OnConnectionDown → s.connected stays true, Health
// lies Full). The fix routes the password path through Reload semantics,
// which REBUILDS the ConnectionManager (Disconnect old + dial fresh), exactly
// like the TLS-rotation path in the same function.
//
// This closes the credentials_refresh_test.go gap: the existing tests never
// touch a LIVE CM.
// ═══════════════════════════════════════════════════════════════════════════

func TestBug_PasswordRotation_OnLiveCM_RebuildsViaReload(t *testing.T) {
	var dialCount atomic.Int32
	var disconnects atomic.Int32

	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "creds-live",
		Username:   "u-old",
		Password:   shared.NewSecret("p-old"),
		// This test exercises the live-CM rebuild-on-rotation mechanics, not
		// the HIGH-4 plaintext gate; opt in so the tcp:// rotation is allowed.
		AllowPlaintextCredentials: true,
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(context.Background()) }()

	// Every dial returns a fresh fake and seeds liveCreds from the current
	// options exactly like the real dial does.
	s.connectOverride = func(_ context.Context) (pahoConnection, context.CancelFunc, error) {
		dialCount.Add(1)
		s.mu.Lock()
		s.liveCreds = mqttCredentials{Username: s.opts.Username, Password: s.opts.Password.Reveal()}
		s.mu.Unlock()
		return &fakeLiveConn{disconnects: &disconnects}, func() {}, nil
	}

	require.NoError(t, s.Start(context.Background()))
	require.Equal(t, int32(1), dialCount.Load(), "first Start dials once")

	// Rotate the password on the LIVE session.
	err := s.ApplyCredentials(context.Background(),
		connectivity.NewCredentialSet(pwCred("u-new", "p-new"), nil))
	require.NoError(t, err)

	// Reload semantics: the old CM is disconnected AND a NEW CM is dialed.
	// The pre-fix path did neither a re-dial nor a rebuild — it merely called
	// cm.Disconnect (terminal) and returned, leaving a dead-but-"connected"
	// session.
	require.Equal(t, int32(2), dialCount.Load(),
		"password rotation must REBUILD the ConnectionManager (Reload), not just Disconnect the live one")
	require.Equal(t, int32(1), disconnects.Load(),
		"the old CM is disconnected exactly once during the rebuild")

	// Health must NOT lie: the session is genuinely connected on the fresh CM.
	h := s.Health(context.Background())
	require.True(t, h.Connected, "session reports connected on the rebuilt CM")

	// liveCreds carry the rotated password for the fresh CONNECT.
	s.mu.Lock()
	got := s.liveCreds
	s.mu.Unlock()
	require.Equal(t, "u-new", got.Username)
	require.Equal(t, "p-new", got.Password)
}

// ═══════════════════════════════════════════════════════════════════════════
// Finding 3 (HIGH): Close/Start race installs a zombie ConnectionManager.
// Start releases s.mu during the (≤30s) AwaitConnection and re-installs the
// CM without re-checking s.closed; Close did not wait for an in-flight Start.
// A Close landing during the connect window would return while Start went on
// to install a CM that autopaho reconnects forever — a zombie fighting the
// replacement session for the ClientID.
//
// Fix: Start re-checks s.closed after dial and tears the fresh CM down if
// closed; Close waits on the startDone signal. Run with -race.
// ═══════════════════════════════════════════════════════════════════════════

// TestBug_ClosedDuringConnectWindow_StartDiscardsCM deterministically
// exercises the primary fix: Start's re-check of s.closed AFTER the dial
// returns. A Close landing during the connect window sets s.closed; the
// re-check must then tear the freshly connected CM down (Disconnect + cancel
// its root context) and return an error rather than install a zombie CM.
func TestBug_ClosedDuringConnectWindow_StartDiscardsCM(t *testing.T) {
	var disconnects, cancelled atomic.Int32
	dialing := make(chan struct{})
	release := make(chan struct{})

	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "close-window",
	}, connectivity.SessionEphemeral, nil)

	s.connectOverride = func(_ context.Context) (pahoConnection, context.CancelFunc, error) {
		close(dialing)
		<-release // block inside the "connect window"
		return &fakeLiveConn{disconnects: &disconnects}, func() { cancelled.Add(1) }, nil
	}

	startErr := make(chan error, 1)
	go func() { startErr <- s.Start(context.Background()) }()

	// Start is blocked inside the dial. Simulate a concurrent Close having
	// won the s.mu race during this window.
	<-dialing
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()

	// Let the dial finish; the post-dial re-check must now discard the CM.
	close(release)

	require.Error(t, <-startErr,
		"Start must fail (not install a zombie CM) when the session was closed during the connect window")
	s.mu.Lock()
	cm := s.cm
	s.mu.Unlock()
	require.Nil(t, cm, "no zombie ConnectionManager may remain installed after a closed-during-connect Start")
	require.Equal(t, int32(1), disconnects.Load(), "the freshly dialed CM was disconnected on teardown")
	require.Equal(t, int32(1), cancelled.Load(), "the freshly dialed CM's root context was cancelled on teardown")
}

// TestBug_CloseDuringStart_NoZombie_Race runs Start and Close concurrently
// (-race) through the real Close path. BOTH interleavings are valid and must
// be zombie-free: either Close wins the s.mu race (Start's re-check tears its
// CM down) or Start wins the install (Close disconnects the installed CM).
// Either way exactly one CM is dialed, disconnected once, its context
// cancelled once, and no CM remains installed.
func TestBug_CloseDuringStart_NoZombie_Race(t *testing.T) {
	var disconnects, cancelled atomic.Int32
	dialing := make(chan struct{})
	release := make(chan struct{})

	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "close-race",
	}, connectivity.SessionEphemeral, nil)

	s.connectOverride = func(_ context.Context) (pahoConnection, context.CancelFunc, error) {
		close(dialing)
		<-release
		return &fakeLiveConn{disconnects: &disconnects}, func() { cancelled.Add(1) }, nil
	}

	startDone := make(chan struct{})
	go func() { _ = s.Start(context.Background()); close(startDone) }()
	<-dialing

	closeDone := make(chan struct{})
	go func() { _ = s.Close(context.Background()); close(closeDone) }()

	close(release)
	<-startDone
	<-closeDone

	s.mu.Lock()
	cm := s.cm
	s.mu.Unlock()
	require.Nil(t, cm, "no zombie ConnectionManager may remain installed after Close (either interleaving)")
	require.Equal(t, int32(1), disconnects.Load(), "the single dialed CM is disconnected exactly once")
	require.Equal(t, int32(1), cancelled.Load(), "the single dialed CM's context is cancelled exactly once")
}

// TestBug_CloseWaitsForInFlightStart asserts Close does not return before an
// in-flight Start has settled (belt-and-braces half of finding 3), so no
// half-built CM can outlive Close.
func TestBug_CloseWaitsForInFlightStart(t *testing.T) {
	dialing := make(chan struct{})
	release := make(chan struct{})
	var disconnects atomic.Int32

	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "close-waits",
	}, connectivity.SessionEphemeral, nil)

	startReturned := make(chan struct{})
	s.connectOverride = func(_ context.Context) (pahoConnection, context.CancelFunc, error) {
		close(dialing)
		<-release
		return &fakeLiveConn{disconnects: &disconnects}, func() {}, nil
	}

	go func() {
		_ = s.Start(context.Background())
		close(startReturned)
	}()
	<-dialing

	closeReturned := make(chan struct{})
	go func() {
		_ = s.Close(context.Background())
		close(closeReturned)
	}()

	// Close must still be blocked on startDone while the dial is held.
	select {
	case <-closeReturned:
		t.Fatal("Close returned while an in-flight Start was still connecting")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-closeReturned:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after the in-flight Start settled")
	}
	<-startReturned
}

// ═══════════════════════════════════════════════════════════════════════════
// FIX 1 (HIGH, security-adjacent): Reload-vs-Start race leaves stale TLS live.
//
// Reload grabbed s.cm under s.mu WITHOUT first awaiting an in-flight Start. A
// supervisor-driven Start (superviseSession re-Run) mid-dial has not installed
// s.cm yet, so Reload saw cm==nil, SKIPPED the teardown of the live connection,
// then called its own Start — which the Start guard makes PIGGYBACK on the
// in-flight dial (returns nil for the second caller). Net: the credential/TLS
// rotation's teardown+rebuild was defeated and the previously-dialed connection
// (carrying the OLD per-dial TLS snapshot) stayed live.
//
// Fix: Reload mirrors Close — snapshot starting+startDone, wait on startDone
// bounded by ctx, THEN grab the now-installed s.cm and tear it down before
// rebuilding.
// ═══════════════════════════════════════════════════════════════════════════

// TestBug_ReloadAwaitsInFlightStart_TearsDownRotatedCM drives a concurrent
// supervisor-style Start (held mid-dial) and a credential-rotation Reload and
// asserts Reload AWAITS the in-flight Start, tears the connection it installed
// down, and rebuilds a fresh one — so no pre-rotation (stale-TLS) connection
// survives.
//
// Counterfactual (proven by removing Reload's in-flight-Start wait): Reload
// snapshots cm==nil, skips teardown, and its Start returns nil on the piggyback
// → only ONE dial happens, s.cm is the FIRST (un-rotated) conn, and it is never
// disconnected. The dialCount==2 assertion below then fails (FailNow before the
// conns[1] access, so no panic).
//
// Run under -race.
func TestBug_ReloadAwaitsInFlightStart_TearsDownRotatedCM(t *testing.T) {
	var dialCount atomic.Int32
	dial1Entered := make(chan struct{})
	dial1Release := make(chan struct{})

	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "reload-await",
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(context.Background()) }()

	var mu sync.Mutex
	var conns []*fakeLiveConn // conns[0]=in-flight-Start dial; conns[1]=Reload's rebuild
	s.connectOverride = func(_ context.Context) (pahoConnection, context.CancelFunc, error) {
		n := dialCount.Add(1)
		c := &fakeLiveConn{disconnects: new(atomic.Int32)}
		mu.Lock()
		conns = append(conns, c)
		mu.Unlock()
		if n == 1 {
			close(dial1Entered)
			<-dial1Release // hold the supervisor Start mid-dial
		}
		return c, func() {}, nil
	}

	// Supervisor-style Start, held mid-dial (s.starting==true, s.cm not yet
	// installed).
	startErr := make(chan error, 1)
	go func() { startErr <- s.Start(context.Background()) }()
	<-dial1Entered

	// Rotation Reload races the in-flight Start. With FIX 1 it WAITS on the
	// in-flight startDone rather than seeing cm==nil and skipping teardown.
	reloadErr := make(chan error, 1)
	go func() { reloadErr <- s.Reload(context.Background()) }()

	// Reload must be PARKED awaiting the in-flight Start — it has not returned
	// while the dial is held. This checkpoint is what makes the counterfactual
	// deterministic: without the wait, Reload has, within this window, already
	// read cm==nil, skipped teardown, and parked in its own piggybacking Start;
	// releasing the dial then yields a piggyback (dialCount stays 1). With the
	// fix, Reload is parked in Reload's own startDone wait and reads cm only
	// AFTER release (dialCount reaches 2).
	wait.Silent(t, reloadErr, 100*time.Millisecond)

	// Release the in-flight Start so it installs conns[0]. Reload, having
	// awaited startDone, then sees conns[0], tears it down, and rebuilds.
	close(dial1Release)

	require.NoError(t, wait.RequireReceive(t, startErr, 3*time.Second),
		"the in-flight Start completes and installs its connection")
	require.NoError(t, wait.RequireReceive(t, reloadErr, 3*time.Second),
		"Reload rebuilds the connection after awaiting the in-flight Start")

	require.Equal(t, int32(2), dialCount.Load(),
		"FIX 1: Reload awaited the in-flight Start, tore its connection down, and RE-DIALED a fresh one")

	mu.Lock()
	first, second := conns[0], conns[1]
	mu.Unlock()

	require.Equal(t, int32(1), first.disconnects.Load(),
		"the connection dialed by the in-flight Start is torn down by the rotation (its stale per-dial TLS cannot survive)")
	require.Equal(t, int32(0), second.disconnects.Load(),
		"the rebuilt connection stays live")

	s.mu.Lock()
	installed := s.cm
	s.mu.Unlock()
	require.True(t, installed == pahoConnection(second),
		"the post-Reload connection manager is the ROTATED one, not the piggybacked in-flight dial")
	require.True(t, installed != pahoConnection(first),
		"the pre-rotation connection is not the installed one")
}
