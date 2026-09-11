//go:build gobridge_amqp10 || gobridge_all

package bootstrap

import (
	"log/slog"

	"github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp10"
	"github.com/mariotoffia/gobridge/ports"
)

func registerAMQP10Decoders(reg *ports.Registry) error { return amqp10.Register(reg) }

// Both discriminators share one factory instance, exactly as the base-set
// transports do: a second instance would open and hold its own broker
// connections for what is one transport.
func wireAMQP10Transports(
	transports map[string]ports.TransportFactory,
	logger *slog.Logger,
	metrics ports.MetricsExporter,
) {
	factory := amqp10.NewFactory(logger, metrics)
	transports["amqp10"] = factory
	transports["amqp.amqp10"] = factory
}
