//go:build !gobridge_azure && !gobridge_all

package bootstrap

import (
	"log/slog"

	"github.com/mariotoffia/gobridge/ports"
)

func registerAzureDecoders(*ports.Registry) error { return nil }

func wireAzureTransports(map[string]ports.TransportFactory, *slog.Logger, ports.MetricsExporter) {
}
