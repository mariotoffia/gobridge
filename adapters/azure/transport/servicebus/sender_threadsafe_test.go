package servicebus

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

// TestSenderEnsureClientConcurrentS14 verifies that concurrent Send calls do not
// race when lazily initializing the client via mutex inside ensureClient.
func TestSenderEnsureClientConcurrentS14(t *testing.T) {
	t.Parallel()

	mock := &mockSenderAPI{}
	sender, err := NewSender(SenderConfig{
		Client:    mock,
		QueueName: "test-queue",
		Connection: ConnectionConfig{
			ConnectionString: "not-used-when-client-injected",
		},
	})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	env := &messaging.Envelope{
		ID:        "id-1",
		Payload:   []byte("payload"),
		CreatedAt: time.Now(),
	}

	const goroutines = 10
	var wg sync.WaitGroup
	errMu := sync.Mutex{}
	var errs []error

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errMu.Lock()
					errs = append(errs, fmt.Errorf("panic: %v", r))
					errMu.Unlock()
				}
			}()
			if err := sender.Send(ctx, ports.OutboundMessage{Envelope: env}); err != nil {
				errMu.Lock()
				errs = append(errs, err)
				errMu.Unlock()
			}
		}()
	}

	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	mock.mu.Lock()
	nSend := len(mock.sentMessages)
	mock.mu.Unlock()
	if nSend != goroutines {
		t.Fatalf("SendMessage calls: want %d, got %d", goroutines, nSend)
	}
}
