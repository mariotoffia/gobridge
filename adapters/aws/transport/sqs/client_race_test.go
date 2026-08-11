package sqs

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
)

// Finding 4 — credential-rotation data race.
//
// The SQS client used to be a plain struct field read unlocked on the hot
// send/receive path while ApplyCredentials/ensureClient swapped it under
// initMu — an unsynchronised read/write data race. The client is now an
// atomic.Pointer snapshot: hot-path readers use loadClient() (lock-free),
// writers serialise the swap under initMu. These tests run the read and
// the swap concurrently; `go test -race` fails if the synchronisation
// regresses to a plain field.

// TestSender_ClientSnapshot_RaceFreeUnderSwap exercises the REAL hot send
// path (sendOne → loadClient().SendMessage) concurrently with client
// swaps performed exactly as ensureClient/ApplyCredentials do (under
// initMu). Mocks keep the path offline and deterministic; the workload is
// bounded (no sleeps).
func TestSender_ClientSnapshot_RaceFreeUnderSwap(t *testing.T) {
	s, err := NewSender(SenderConfig{
		Region:   "us-east-1",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123/q",
		Client:   &mockSQSClient{},
	})
	require.NoError(t, err)
	require.NoError(t, s.ensureClient(context.Background()))

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "race", Payload: []byte("{}")})

	var readers, writers sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = s.sendOne(context.Background(), env)
				}
			}
		}()
	}

	for i := 0; i < 4; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for j := 0; j < 500; j++ {
				s.initMu.Lock()
				s.storeClient(&mockSQSClient{})
				s.initMu.Unlock()
			}
		}()
	}

	writers.Wait()
	close(stop)
	readers.Wait()

	require.NotNil(t, s.loadClient())
}

// TestReceiver_ApplyCredentials_RaceFreeUnderRead runs the REAL
// ApplyCredentials rotation (which builds a fresh *sqs.Client and swaps it
// under initMu) concurrently with lock-free snapshot reads. Readers only
// load the snapshot — they never call a method on it — so a swapped-in real
// client never performs network I/O.
func TestReceiver_ApplyCredentials_RaceFreeUnderRead(t *testing.T) {
	r, err := NewReceiver(ReceiverConfig{
		Region:   "us-east-1",
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123/q",
		Client:   &mockSQSClient{},
	}, nil)
	require.NoError(t, err)
	require.NoError(t, r.ensureClient(context.Background()))

	var readers, writers sync.WaitGroup
	stop := make(chan struct{})
	var reads int64

	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if r.loadClient() != nil {
						atomic.AddInt64(&reads, 1)
					}
				}
			}
		}()
	}

	for i := 0; i < 2; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for j := 0; j < 20; j++ {
				pw := connectivity.NewPasswordCredential("AKIA", "SECRET")
				_ = r.ApplyCredentials(context.Background(), connectivity.NewCredentialSet(&pw, nil))
			}
		}()
	}

	writers.Wait()
	close(stop)
	readers.Wait()

	require.NotNil(t, r.loadClient())
	require.Positive(t, atomic.LoadInt64(&reads))
}
