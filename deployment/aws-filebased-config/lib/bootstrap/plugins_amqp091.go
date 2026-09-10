//go:build gobridge_amqp091 || gobridge_all

package bootstrap

import (
	"log/slog"

	"github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091"
	"github.com/mariotoffia/gobridge/ports"
)

func registerAMQP091Decoders(reg *ports.Registry) error { return amqp091.Register(reg) }

// Both discriminators share one factory instance, exactly as the base-set
// transports do: a second instance would open and hold its own broker
// connections for what is one transport.
func wireAMQP091Transports(
	transports map[string]ports.TransportFactory,
	logger *slog.Logger,
	metrics ports.MetricsExporter,
) {
	factory := amqp091.NewFactory(logger, metrics)
	transports["amqp091"] = factory
	transports["amqp.amqp091"] = factory
}
