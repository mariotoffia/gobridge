// ═══════════════════════════════════════════════
// Production-readiness remediation tests: credential-rotation data race.
//
// Covers the MEDIUM finding — applyAMQPTLSMaterial mutated the shared
// *TLSConfig in place and ApplyCredentials reassigned s.dial, while the
// reconnect path (dialWithTimeout) read both without the lock. Rotation
// concurrent with an in-flight reconnect was a data race that could hand
// the SDK a torn cert/key pair. The fix is copy-on-write TLS material plus
// a locked snapshot of s.dial before the dial goroutine spawns.
//
// This test MUST be -race clean.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// testTLSReadingDial mirrors defaultDialFromOpts's capture semantics: it
// snapshots opts by value (opts.TLS is a pointer) and reads the TLS material
// fields when dialed — exactly the concurrent read the copy-on-write fix
// must keep race-free. It never opens a socket, returning a fresh mock
// connection instead so the test is hermetic.
func testTLSReadingDial(opts SessionOptions) dialFunc {
	return func(string) (amqpConnection, error) {
		if opts.TLS != nil {
			_ = opts.TLS.Enable
			_ = opts.TLS.InsecureSkipVerify
			_ = opts.TLS.CertPEM.Reveal()
			_ = opts.TLS.KeyPEM.Reveal()
			_ = opts.TLS.CACertPEM.Reveal()
		}
		return newMockConnection(), nil
	}
}

// TestSession_CredentialRotation_ConcurrentDial_RaceClean drives credential
// rotation (real ApplyCredentials) concurrently with the reconnect dial path
// (real dialWithTimeout) and must be -race clean. Without the copy-on-write
// TLS swap and the locked dial snapshot the race detector fires on the shared
// *TLSConfig fields and the s.dial field.
func TestSession_CredentialRotation_ConcurrentDial_RaceClean(t *testing.T) {
	s := newResilienceSession(nil)
	// Hermetic dialer that reads TLS material; both the initial dial and the
	// rotation-rebuilt dial go through it (never the SDK/network path).
	s.dialBuilder = testTLSReadingDial
	s.opts.TLS = &TLSConfig{
		Enable:  true,
		CertPEM: shared.NewSecret("cert-0"),
		KeyPEM:  shared.NewSecret("key-0"),
	}
	s.dial = testTLSReadingDial(s.opts)
	t.Cleanup(func() { _ = sessionCloseQuiet(s) })

	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(2)

	// Reconnect/dial reader: repeatedly snapshots s.dial under the lock and
	// dials, reading the captured TLS material.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			if _, err := s.dialWithTimeout(ctx); err != nil {
				cancel()
				continue
			}
			cancel()
		}
	}()

	// Rotator: repeatedly rotates the TLS cert/key, which copy-on-writes the
	// *TLSConfig and rebuilds s.dial under the lock.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			cert := fmt.Sprintf("cert-%d", i+1)
			key := fmt.Sprintf("key-%d", i+1)
			set := connectivity.NewCredentialSet(nil, tlsMat(cert, key, nil, false))
			if err := s.ApplyCredentials(context.Background(), set); err != nil {
				t.Errorf("ApplyCredentials: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}

// sessionCloseQuiet closes the session ignoring the error; the resilience
// session was never Start()ed so Close only flips flags and drains channels.
func sessionCloseQuiet(s *Session) error {
	return s.Close(context.Background())
}
