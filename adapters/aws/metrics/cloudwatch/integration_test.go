package cloudwatch

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	cwsdk "github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/mariotoffia/gobridge/testutil/flocilocal"
)

func TestMain(m *testing.M) {
	flocilocal.Configure(flocilocal.WithCleanOrphans(true))
	code := m.Run()
	flocilocal.Shutdown()
	os.Exit(code)
}

func integrationClient(t testing.TB) *cwsdk.Client {
	t.Helper()
	ep := flocilocal.Endpoint(t)
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-west-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", ""),
		),
	)
	if err != nil {
		t.Fatalf("load AWS config: %v", err)
	}
	return cwsdk.NewFromConfig(cfg, func(o *cwsdk.Options) {
		o.BaseEndpoint = aws.String(ep)
	})
}

func uniqueNamespace(prefix string) string {
	return fmt.Sprintf("%s/%d", prefix, time.Now().UnixNano())
}

// Verifies PutMetricData succeeds against the AWS emulator and the metric appears in ListMetrics.
func TestIntegration_PutMetricData(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	ns := uniqueNamespace("Test/PutMetric")

	_, err := client.PutMetricData(ctx, &cwsdk.PutMetricDataInput{
		Namespace: aws.String(ns),
		MetricData: []cwtypes.MetricDatum{{
			MetricName: aws.String("TestCounter"),
			Value:      aws.Float64(42),
			Unit:       cwtypes.StandardUnitCount,
		}},
	})
	if err != nil {
		t.Fatalf("PutMetricData: %v", err)
	}

	out, err := client.ListMetrics(ctx, &cwsdk.ListMetricsInput{
		Namespace: aws.String(ns),
	})
	if err != nil {
		t.Fatalf("ListMetrics: %v", err)
	}
	if len(out.Metrics) == 0 {
		t.Fatal("expected at least 1 metric in namespace")
	}

	found := false
	for _, m := range out.Metrics {
		if *m.MetricName == "TestCounter" {
			found = true
		}
	}
	if !found {
		t.Error("expected metric TestCounter in listing")
	}
}

// Verifies the Exporter flushes counters to the AWS emulator and they appear in ListMetrics.
func TestIntegration_Exporter_FlushCounters(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	ns := uniqueNamespace("Test/Exporter")

	e, err := New(ctx, ns,
		WithClient(client),
		WithFlushInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close(ctx) }()

	e.Counter("integration.requests", 10)
	e.Counter("integration.errors", 2)

	if err := e.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	out, err := client.ListMetrics(ctx, &cwsdk.ListMetricsInput{
		Namespace: aws.String(ns),
	})
	if err != nil {
		t.Fatalf("ListMetrics: %v", err)
	}

	names := map[string]bool{}
	for _, m := range out.Metrics {
		names[*m.MetricName] = true
	}
	if !names["integration.requests"] {
		t.Error("expected integration.requests metric")
	}
	if !names["integration.errors"] {
		t.Error("expected integration.errors metric")
	}
}

// Verifies the Exporter flushes histograms (StatisticValues) to the AWS emulator.
func TestIntegration_Exporter_FlushHistograms(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	ns := uniqueNamespace("Test/Histogram")

	e, err := New(ctx, ns,
		WithClient(client),
		WithFlushInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Close(ctx) }()

	e.Histogram("integration.latency", 10)
	e.Histogram("integration.latency", 20)
	e.Histogram("integration.latency", 30)

	if err := e.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	out, err := client.ListMetrics(ctx, &cwsdk.ListMetricsInput{
		Namespace: aws.String(ns),
	})
	if err != nil {
		t.Fatalf("ListMetrics: %v", err)
	}

	found := false
	for _, m := range out.Metrics {
		if *m.MetricName == "integration.latency" {
			found = true
		}
	}
	if !found {
		t.Error("expected integration.latency metric")
	}
}

// Verifies EnsureAlarms creates alarms visible via DescribeAlarms on the AWS emulator.
func TestIntegration_EnsureAlarms(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	ns := uniqueNamespace("Test/Alarms")

	alarms := []AlarmDefinition{
		{
			Name:        fmt.Sprintf("test-alarm-1-%d", time.Now().UnixNano()),
			MetricName:  "TestMetric",
			Namespace:   ns,
			Threshold:   100,
			Period:      60,
			EvalPeriods: 1,
			Statistic:   cwtypes.StatisticSum,
			Comparison:  cwtypes.ComparisonOperatorGreaterThanThreshold,
			Severity:    SeverityWarning,
		},
		{
			Name:        fmt.Sprintf("test-alarm-2-%d", time.Now().UnixNano()),
			MetricName:  "TestMetric",
			Namespace:   ns,
			Threshold:   500,
			Period:      300,
			EvalPeriods: 2,
			Statistic:   cwtypes.StatisticAverage,
			Comparison:  cwtypes.ComparisonOperatorGreaterThanOrEqualToThreshold,
			Severity:    SeverityCritical,
		},
	}

	if err := EnsureAlarms(ctx, client, alarms); err != nil {
		t.Fatalf("EnsureAlarms: %v", err)
	}

	for _, a := range alarms {
		out, err := client.DescribeAlarms(ctx, &cwsdk.DescribeAlarmsInput{
			AlarmNames: []string{a.Name},
		})
		if err != nil {
			t.Fatalf("DescribeAlarms %s: %v", a.Name, err)
		}
		if len(out.MetricAlarms) != 1 {
			t.Fatalf("expected 1 alarm for %s, got %d", a.Name, len(out.MetricAlarms))
		}
		alarm := out.MetricAlarms[0]
		if *alarm.Namespace != ns {
			t.Errorf("alarm %s: Namespace = %q, want %q", a.Name, *alarm.Namespace, ns)
		}
		if *alarm.Threshold != a.Threshold {
			t.Errorf("alarm %s: Threshold = %f, want %f", a.Name, *alarm.Threshold, a.Threshold)
		}
	}
}

// Verifies EnsureAlarms is idempotent (can be called twice without error).
func TestIntegration_EnsureAlarms_Idempotent(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	ns := uniqueNamespace("Test/Idempotent")

	alarms := []AlarmDefinition{{
		Name:        fmt.Sprintf("idempotent-alarm-%d", time.Now().UnixNano()),
		MetricName:  "M",
		Namespace:   ns,
		Threshold:   10,
		Period:      60,
		EvalPeriods: 1,
		Statistic:   cwtypes.StatisticSum,
		Comparison:  cwtypes.ComparisonOperatorGreaterThanThreshold,
		Severity:    SeverityWarning,
	}}

	if err := EnsureAlarms(ctx, client, alarms); err != nil {
		t.Fatalf("EnsureAlarms (first): %v", err)
	}
	if err := EnsureAlarms(ctx, client, alarms); err != nil {
		t.Fatalf("EnsureAlarms (second): %v", err)
	}
}

// Verifies Close performs a final flush to the AWS emulator.
func TestIntegration_Exporter_Close(t *testing.T) {
	client := integrationClient(t)
	ctx := context.Background()
	ns := uniqueNamespace("Test/Close")

	e, err := New(ctx, ns,
		WithClient(client),
		WithFlushInterval(time.Hour),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	e.Counter("close.metric", 1)

	if err := e.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out, err := client.ListMetrics(ctx, &cwsdk.ListMetricsInput{
		Namespace: aws.String(ns),
	})
	if err != nil {
		t.Fatalf("ListMetrics: %v", err)
	}

	found := false
	for _, m := range out.Metrics {
		if *m.MetricName == "close.metric" {
			found = true
		}
	}
	if !found {
		t.Error("expected close.metric after Close()")
	}
}
