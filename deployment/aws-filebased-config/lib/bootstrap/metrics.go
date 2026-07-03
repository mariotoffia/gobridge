package bootstrap

import (
	"context"
	"fmt"
	"log/slog"

	cwmetrics "github.com/mariotoffia/gobridge/adapters/aws/metrics/cloudwatch"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/ports"
)

// newMetricsExporter builds the runtime metrics exporter selected by the
// bootstrap config's MetricsExporter field. It returns:
//
//   - (nil, nil) for the default noop selection ("" or "noop") — the App
//     then registers no bridge.WithMetrics option, preserving today's
//     zero-metrics behaviour.
//   - (exporter, nil) for "cloudwatch" — a live CloudWatch exporter wired
//     with the default rollup metrics (so the declarative alarms match its
//     zero-dimension rollup series, MF-4) and the per-instance dimension.
//   - (nil, err) for any unknown value — fail fast (Validate already rejects
//     these before Start reaches here; the case is retained defensively so
//     the selection is total).
//
// The returned exporter owns a background flush goroutine and MUST be Closed
// on shutdown (App.Stop) to avoid a goroutine leak.
func newMetricsExporter(ctx context.Context, cfg deployinfra.BootstrapConfig, logger *slog.Logger) (ports.MetricsExporter, error) {
	switch cfg.MetricsExporter {
	case "", deployinfra.MetricsExporterNoop:
		return nil, nil

	case deployinfra.MetricsExporterCloudWatch:
		opts := []cwmetrics.Option{
			// Double-publish a zero-dimension rollup copy so the
			// declarative CloudWatch alarms (which carry no dimensions)
			// actually match the emitted series (MF-4).
			cwmetrics.WithRollupMetrics(cwmetrics.DefaultRollupMetrics()...),
			// Empty InstanceID derives "<hostname>-<pid>", already unique
			// per Fargate task; a set value gives a deterministic identity.
			cwmetrics.WithInstanceTag(cfg.InstanceID),
			cwmetrics.WithLogger(logger),
		}
		if cfg.AWSRegion != "" {
			opts = append(opts, cwmetrics.WithRegion(cfg.AWSRegion))
		}
		exporter, err := cwmetrics.New(ctx, cfg.EffectiveMetricsNamespace(), opts...)
		if err != nil {
			return nil, fmt.Errorf("bootstrap: build cloudwatch metrics exporter: %w", err)
		}
		return exporter, nil

	default:
		return nil, fmt.Errorf("bootstrap: unsupported metrics_exporter %q (want \"\", %q, or %q)",
			cfg.MetricsExporter, deployinfra.MetricsExporterNoop, deployinfra.MetricsExporterCloudWatch)
	}
}
