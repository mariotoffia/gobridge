//go:build gobridge_azure || gobridge_all

package bootstrap

import (
	"log/slog"

	"github.com/mariotoffia/gobridge/adapters/azure/transport/servicebus"
	"github.com/mariotoffia/gobridge/ports"
)

func registerAzureDecoders(reg *ports.Registry) error { return servicebus.Register(reg) }

// The Service Bus factory is logger-only; it carries no metrics seam, so the
// exporter this profile builds has nothing to attach to here.
// Both discriminators share one factory instance, exactly as the base-set
// transports do.
func wireAzureTransports(
	transports map[string]ports.TransportFactory,
	logger *slog.Logger,
	_ ports.MetricsExporter,
) {
	factory := servicebus.NewFactory(logger)
	transports["servicebus"] = factory
	transports["azure.servicebus"] = factory
}
