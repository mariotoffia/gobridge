//go:build !gobridge_amqp091 && !gobridge_all

package bootstrap

import (
	"log/slog"

	"github.com/mariotoffia/gobridge/ports"
)

func registerAMQP091Decoders(*ports.Registry) error { return nil }

func wireAMQP091Transports(map[string]ports.TransportFactory, *slog.Logger, ports.MetricsExporter) {
}
