package servicebus_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/adapters/azure/transport/servicebus"
	"github.com/mariotoffia/gobridge/ports"
)

// A receiver pinned to one Service Bus session holds a broker-side session
// lock that no second receiver can take. A reconfiguration that overlaps the
// old and new runtimes therefore races: the outgoing receiver keeps the lock
// for as long as its drain runs, while the incoming one retries
// session-cannot-be-locked on a shorter budget and then fails the route
// terminally. Reporting the pin from the config lets the swap serialize
// instead, before either receiver exists.
//
// Category: unit (TESTS.md §1).
func TestFactory_ConfigRequiresExclusiveIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  ports.PluginConfig
		want bool
	}{
		{
			name: "a pinned session is an exclusive identity",
			cfg:  &servicebus.Config{Receiver: servicebus.ReceiverParams{QueueName: "q", SessionID: "s-1"}},
			want: true,
		},
		{
			name: "a pinned session by value is an exclusive identity",
			cfg:  servicebus.Config{Receiver: servicebus.ReceiverParams{QueueName: "q", SessionID: "s-1"}},
			want: true,
		},
		{
			name: "rotating sessions pin nothing, so two receivers can coexist",
			cfg:  &servicebus.Config{Receiver: servicebus.ReceiverParams{QueueName: "q", UseSessions: true}},
			want: false,
		},
		{
			name: "a plain queue receiver shares the queue",
			cfg:  &servicebus.Config{Receiver: servicebus.ReceiverParams{QueueName: "q"}},
			want: false,
		},
		{
			name: "an undecodable config claims nothing",
			cfg:  nil,
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, servicebus.NewFactory(nil).ConfigRequiresExclusiveIdentity(tc.cfg))
		})
	}
}
