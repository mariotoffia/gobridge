//go:build !gobridge_amqp10 && !gobridge_all

package bootstrap

import (
	"log/slog"

	"github.com/mariotoffia/gobridge/ports"
)

func registerAMQP10Decoders(*ports.Registry) error { return nil }

func wireAMQP10Transports(map[string]ports.TransportFactory, *slog.Logger, ports.MetricsExporter) {
}
