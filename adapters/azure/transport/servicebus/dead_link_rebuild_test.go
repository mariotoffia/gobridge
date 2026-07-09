package servicebus

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/ports"
)

// deadLinkBuilder is a buildStackFn seam whose FIRST built receiver has a
// terminally-CLOSED link (ReceiveMessages returns a typed CodeClosed error)
// and whose subsequent builds are healthy-but-idle. It counts builds so a
// test can assert the poll loop actually rebuilt.
type deadLinkBuilder struct {
	mu     sync.Mutex
	builds int
}

func (b *deadLinkBuilder) build(context.Context, ConnectionConfig) (receiverStack, error) {
	b.mu.Lock()
	b.builds++
	n := b.builds
	b.mu.Unlock()

	c := &closeableASBClient{}
	if n == 1 {
		c.ReceiveMessagesFn = func(context.Context, int, *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			// A TYPED *azservicebus.Error with CodeClosed — the local link
			// was closed. isClosedLinkError classifies this as the terminal
			// state a non-session receiver must rebuild out of (a plain
			// string is deliberately NOT enough).
			return nil, &azservicebus.Error{Code: azservicebus.CodeClosed}
		}
	} else {
		c.ReceiveMessagesFn = func(context.Context, int, *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			return nil, nil // healthy, idle
		}
	}
	return receiverStack{client: c}, nil
}

func (b *deadLinkBuilder) buildCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builds
}

// --- c6-dead-link: non-session receiver rebuilds a closed link -------------

// TestReceiver_NonSession_RebuildsDeadLinkOnClosedLink proves the
// non-session poll loop REBUILDS a terminally-CLOSED receiver instead of
// re-polling the same dead link forever. The first stack's ReceiveMessages
// returns a typed *azservicebus.Error{Code: CodeClosed} — a locally closed
// AMQP link, which never recovers on its own — and the loop must build a
// second stack. (CodeConnectionLost is deliberately NOT a trigger: the SDK
// self-heals it by reopening the connection on the next ReceiveMessages, so
// rebuilding on it would race that recovery — see isClosedLinkError.)
//
// Mutation: without the rebuild branch the loop only counts the failure,
// backs off, and re-polls the SAME (dead) stack, so buildCount never
// exceeds 1 and the wait times out.
func TestReceiver_NonSession_RebuildsDeadLinkOnClosedLink(t *testing.T) {
	t.Parallel()

	builder := &deadLinkBuilder{}
	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "q",
		Connection: ConnectionConfig{Namespace: "ns.servicebus.windows.net"},
		Clock:      clocktest.New(),
	}, nil)
	require.NoError(t, err)
	recv.buildStackFn = builder.build

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() {
		runErr <- recv.Run(ctx, func(context.Context, ports.Delivery) error { return nil })
	}()

	waitUntil(t, 3*time.Second, func() bool { return builder.buildCount() >= 2 },
		"non-session receiver did not rebuild its closed link after a CodeClosed error")

	cancel()
	require.ErrorIs(t, <-runErr, context.Canceled)
}

// --- c6-dead-link (fix #5): rebuild trigger is the TYPED closed-link code --

// TestIsClosedLinkError_OnlyTypedCodeClosedTriggersRebuild pins the rebuild
// trigger to the SDK's typed *azservicebus.Error{Code: CodeClosed} and to
// NOTHING else. This is the exact classification that decides whether the
// non-session poll loop rebuilds (fix #4) — a false positive on a permanent
// auth/config fault would mask the real cause behind an endless rebuild
// loop; a false positive on the self-healing CodeConnectionLost would race
// the SDK's own recovery.
//
// Mutation: revert the trigger to MapError's substring-first classification
// (matches "connection" before "unauthorized"/"invalid"). Then the
// "invalid connection string" and "amqp: connection reset" cases classify
// as connection-lost → isClosedLinkError would report true → those two
// subtests FAIL.
func TestIsClosedLinkError_OnlyTypedCodeClosedTriggersRebuild(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "typed CodeClosed rebuilds",
			err:  &azservicebus.Error{Code: azservicebus.CodeClosed},
			want: true,
		},
		{
			name: "typed CodeConnectionLost self-heals, no rebuild",
			err:  &azservicebus.Error{Code: azservicebus.CodeConnectionLost},
			want: false,
		},
		{
			name: "permanent auth fault must NOT rebuild-loop",
			err:  errors.New("invalid connection string; missing SharedAccessKey"),
			want: false,
		},
		{
			name: "substring 'connection reset' is not the typed closed code",
			err:  errors.New("amqp: connection reset by peer"),
			want: false,
		},
		{
			name: "nil is never a rebuild trigger",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, isClosedLinkError(tc.err))
		})
	}
}
