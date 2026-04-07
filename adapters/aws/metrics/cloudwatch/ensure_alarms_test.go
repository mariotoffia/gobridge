package cloudwatch

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// Verifies EnsureAlarms issues one PutMetricAlarm call per definition.
func TestEnsureAlarms_CreatesAlarms(t *testing.T) {
	mock := &mockCloudWatch{}
	alarms := DefaultAlarms("TestNS", "arn:aws:sns:us-west-1:123:topic")

	err := EnsureAlarms(context.Background(), mock, alarms)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	calls := mock.metricAlarmCalls()
	if len(calls) != len(alarms) {
		t.Fatalf("expected %d PutMetricAlarm calls, got %d", len(alarms), len(calls))
	}

	for i, call := range calls {
		if *call.AlarmName != alarms[i].Name {
			t.Errorf("call[%d] AlarmName = %q, want %q", i, *call.AlarmName, alarms[i].Name)
		}
		if *call.MetricName != alarms[i].MetricName {
			t.Errorf("call[%d] MetricName = %q, want %q", i, *call.MetricName, alarms[i].MetricName)
		}
		if *call.Namespace != alarms[i].Namespace {
			t.Errorf("call[%d] Namespace = %q, want %q", i, *call.Namespace, alarms[i].Namespace)
		}
		if *call.Threshold != alarms[i].Threshold {
			t.Errorf("call[%d] Threshold = %f, want %f", i, *call.Threshold, alarms[i].Threshold)
		}
		if call.Statistic != alarms[i].Statistic {
			t.Errorf("call[%d] Statistic = %v, want %v", i, call.Statistic, alarms[i].Statistic)
		}
		if call.ComparisonOperator != alarms[i].Comparison {
			t.Errorf("call[%d] Comparison = %v, want %v", i, call.ComparisonOperator, alarms[i].Comparison)
		}
	}
}

// Verifies EnsureAlarms sets AlarmActions and OKActions when an SNS ARN is provided.
func TestEnsureAlarms_SNSActions(t *testing.T) {
	mock := &mockCloudWatch{}
	arn := "arn:aws:sns:eu-west-1:123:alerts"
	alarms := []AlarmDefinition{{
		Name:        "test-alarm",
		MetricName:  "TestMetric",
		Namespace:   "Test",
		Threshold:   10,
		Period:      300,
		EvalPeriods: 1,
		Statistic:   cwtypes.StatisticSum,
		Comparison:  cwtypes.ComparisonOperatorGreaterThanThreshold,
		Severity:    SeverityWarning,
		SNSTopicARN: arn,
	}}

	if err := EnsureAlarms(context.Background(), mock, alarms); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	calls := mock.metricAlarmCalls()
	if len(calls[0].AlarmActions) != 1 || calls[0].AlarmActions[0] != arn {
		t.Errorf("AlarmActions = %v, want [%s]", calls[0].AlarmActions, arn)
	}
	if len(calls[0].OKActions) != 1 || calls[0].OKActions[0] != arn {
		t.Errorf("OKActions = %v, want [%s]", calls[0].OKActions, arn)
	}
}

// Verifies EnsureAlarms omits alarm/ok actions when no SNS ARN is given.
func TestEnsureAlarms_NoSNS(t *testing.T) {
	mock := &mockCloudWatch{}
	alarms := []AlarmDefinition{{
		Name:        "test-alarm",
		MetricName:  "TestMetric",
		Namespace:   "Test",
		Threshold:   5,
		Period:      60,
		EvalPeriods: 1,
		Statistic:   cwtypes.StatisticAverage,
		Comparison:  cwtypes.ComparisonOperatorGreaterThanOrEqualToThreshold,
		Severity:    SeverityCritical,
	}}

	if err := EnsureAlarms(context.Background(), mock, alarms); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	calls := mock.metricAlarmCalls()
	if len(calls[0].AlarmActions) != 0 {
		t.Errorf("expected no AlarmActions, got %v", calls[0].AlarmActions)
	}
	if len(calls[0].OKActions) != 0 {
		t.Errorf("expected no OKActions, got %v", calls[0].OKActions)
	}
}

// Verifies EnsureAlarms is a no-op for an empty alarm slice.
func TestEnsureAlarms_EmptySlice(t *testing.T) {
	mock := &mockCloudWatch{}

	if err := EnsureAlarms(context.Background(), mock, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.metricAlarmCalls()) != 0 {
		t.Error("expected no PutMetricAlarm calls for empty input")
	}
}

// Verifies EnsureAlarms propagates API errors and stops on first failure.
func TestEnsureAlarms_APIError(t *testing.T) {
	apiErr := errors.New("access denied")
	mock := &mockCloudWatch{
		PutMetricAlarmFn: func(ctx context.Context, params *cloudwatch.PutMetricAlarmInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricAlarmOutput, error) {
			return nil, apiErr
		},
	}

	alarms := DefaultAlarms("Test", "")
	err := EnsureAlarms(context.Background(), mock, alarms)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, apiErr) {
		t.Errorf("expected wrapped apiErr, got %v", err)
	}

	if len(mock.metricAlarmCalls()) != 1 {
		t.Errorf("expected 1 call (stop on first error), got %d", len(mock.metricAlarmCalls()))
	}
}

// Verifies EnsureAlarms sets TreatMissingData and AlarmDescription.
func TestEnsureAlarms_AlarmMetadata(t *testing.T) {
	mock := &mockCloudWatch{}
	alarms := []AlarmDefinition{{
		Name:        "meta-alarm",
		MetricName:  "M",
		Namespace:   "NS",
		Threshold:   1,
		Period:      60,
		EvalPeriods: 2,
		Statistic:   cwtypes.StatisticSum,
		Comparison:  cwtypes.ComparisonOperatorGreaterThanThreshold,
		Severity:    SeverityCritical,
	}}

	if err := EnsureAlarms(context.Background(), mock, alarms); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	call := mock.metricAlarmCalls()[0]
	if *call.TreatMissingData != "notBreaching" {
		t.Errorf("TreatMissingData = %q, want notBreaching", *call.TreatMissingData)
	}
	if call.AlarmDescription == nil || *call.AlarmDescription == "" {
		t.Error("expected non-empty AlarmDescription")
	}
	if *call.EvaluationPeriods != 2 {
		t.Errorf("EvaluationPeriods = %d, want 2", *call.EvaluationPeriods)
	}
}
