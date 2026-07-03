// Package cloudwatch implements [ports.MetricsExporter] for AWS CloudWatch.
//
// It buffers counter, gauge, histogram, and timer metrics in memory and
// flushes them periodically to CloudWatch via PutMetricData. Counters
// are aggregated per (name, tags) within the flush window; histograms
// and timers are aggregated into StatisticSet values (min/max/sum/count)
// to reduce the number of API calls. Batches respect the real
// PutMetricData API limits (1000 datums / ~1MB per request).
//
// A single long-lived flusher goroutine drains the buffer; the emission
// path never spawns goroutines and never blocks the caller. The buffer
// is hard-capped (WithMaxBufferedDatums); when full, new series are
// dropped and counted while existing aggregating series keep
// accumulating. NaN/±Inf values are rejected at emission.
//
// Flush errors are classified: permanent (client-fault) errors drop the
// offending batch so a poison datum cannot brick the pipeline, while
// throttling/server errors are requeued with backoff. Self-loss is
// visible two ways: Warn logs via slog.Default() (opt out with
// WithLogger(nil)) and the zero-dimension self-metrics
// ExporterDroppedDatums / ExporterRejectedDatums published through the
// exporter's own pipeline.
//
// CloudWatch alarms on a metric without dimensions never match
// dimensioned data. WithRollupMetrics double-publishes the listed
// metrics without dimensions so fleet-wide alarms (see DefaultAlarms
// and DefaultRollupMetrics) can target them. WithInstanceTag stamps
// every datum with an instance_id dimension so per-instance series in a
// fleet do not collide.
//
// Usage:
//
//	exporter, err := cloudwatch.New(ctx, "GoBridge/Runtime",
//	    cloudwatch.WithRegion("eu-west-1"),
//	    cloudwatch.WithFlushInterval(30*time.Second),
//	    cloudwatch.WithRollupMetrics(cloudwatch.DefaultRollupMetrics()...),
//	    cloudwatch.WithInstanceTag(settings.InstanceID),
//	)
//	if err != nil { ... }
//	defer exporter.Close(ctx)
//
//	runtime := runtime.New(
//	    runtime.WithMetrics(exporter),
//	)
package cloudwatch
