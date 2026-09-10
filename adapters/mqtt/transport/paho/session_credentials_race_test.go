package paho

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// ═══════════════════════════════════════════════════════════════════════════
// Race regression: ApplyCredentials used to mutate s.opts (username,
// password, TLS PEM fields) in place WITHOUT the session mutex, racing
// a supervisor-restarted Start that reads the TLS material to build the
// tls.Config — a torn TLS config under -race. The fix moves all
// mutations under s.mu and makes TLS updates copy-on-write pointer
// swaps, so Start's snapshot of s.opts.TLS is immutable.
//
// These tests are only meaningful under `go test -race` — they drive
// the two paths concurrently and let the race detector be the oracle.
// ═══════════════════════════════════════════════════════════════════════════

// TestApplyCredentials_ConcurrentWithStart_NoRace hammers rotation
// (password + TLS material) while Start attempts run against an
// unreachable broker. The TLS PEM material is garbage, so Start fails
// fast at BuildTLSConfig — but only AFTER reading the snapshot, which
// is exactly the historical race window.
func TestApplyCredentials_ConcurrentWithStart_NoRace(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"}, // TEST-NET, unreachable
		ClientID:   "creds-race",
		Username:   "u0",
		Password:   shared.NewSecret("p0"),
		// The point of this test is the -race window on the s.opts mutation;
		// opt in to the plaintext gate so the rotation ACTUALLY mutates (a
		// gate-rejected rotation would leave s.opts unchanged and make the
		// race window vacuous). The gate itself is covered elsewhere.
		AllowPlaintextCredentials: true,
		TLS: &TLSConfig{
			Enable:  true,
			CertPEM: shared.NewSecret("not-a-cert-0"),
			KeyPEM:  shared.NewSecret("not-a-key-0"),
		},
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(context.Background()) }()

	const iterations = 50
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			pw := pwCred(fmt.Sprintf("u%d", i), fmt.Sprintf("p%d", i))
			mat := tlsMat(
				fmt.Sprintf("not-a-cert-%d", i),
				fmt.Sprintf("not-a-key-%d", i),
				[]string{fmt.Sprintf("not-a-ca-%d", i)},
				i%2 == 0,
			)
			// The session has no live cm, so rotation stashes the new
			// material on opts (pointer swap) without reconnecting.
			_ = s.ApplyCredentials(context.Background(), connectivity.NewCredentialSet(pw, mat))
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			// Start reads the TLS snapshot and fails on the garbage
			// PEM (or times out dialing). Both outcomes are fine — the
			// race detector is the assertion here.
			_ = s.Start(ctx)
			cancel()
		}
	}()

	wg.Wait()
}

// TestApplyCredentials_TLSCopyOnWrite verifies rotation swaps in a NEW
// *TLSConfig and never mutates the one a concurrent Start may have
// snapshotted.
func TestApplyCredentials_TLSCopyOnWrite(t *testing.T) {
	orig := &TLSConfig{
		Enable:  true,
		CertPEM: shared.NewSecret("cert-old"),
		KeyPEM:  shared.NewSecret("key-old"),
	}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "creds-cow",
		TLS:        orig,
	}, connectivity.SessionEphemeral, nil)
	defer func() { _ = s.Close(context.Background()) }()

	err := s.ApplyCredentials(t.Context(), connectivity.NewCredentialSet(
		nil, tlsMat("cert-new", "key-new", nil, false)))
	require.NoError(t, err)

	s.mu.Lock()
	cur := s.opts.TLS
	s.mu.Unlock()

	require.NotSame(t, orig, cur, "rotation must swap in a new TLSConfig, not mutate in place")
	require.Equal(t, "cert-old", orig.CertPEM.Reveal(), "snapshotted config must be untouched")
	require.Equal(t, "key-old", orig.KeyPEM.Reveal())
	require.Equal(t, "cert-new", cur.CertPEM.Reveal())
	require.Equal(t, "key-new", cur.KeyPEM.Reveal())
}
