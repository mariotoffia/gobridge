// Package cloudwatch implements [ports.MetricsExporter] for AWS CloudWatch.
//
// It buffers counter, gauge, histogram, and timer metrics in memory and
// flushes them periodically to CloudWatch via PutMetricData. Histograms
// and timers are aggregated into StatisticSet values (min/max/sum/count)
// to reduce the number of API calls.
//
// Usage:
//
//	exporter, err := cloudwatch.New(ctx, "GoBridge/Runtime",
//	    cloudwatch.WithRegion("eu-west-1"),
//	    cloudwatch.WithFlushInterval(30*time.Second),
//	)
//	if err != nil { ... }
//	defer exporter.Close(ctx)
//
//	runtime := runtime.New(
//	    runtime.WithMetrics(exporter),
//	)
package cloudwatch
