package cloudwatch

import (
	"context"
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
)

type mockCloudWatch struct {
	mu sync.Mutex

	PutMetricDataFn  func(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error)
	PutMetricAlarmFn func(ctx context.Context, params *cloudwatch.PutMetricAlarmInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricAlarmOutput, error)

	putMetricDataCalls  []*cloudwatch.PutMetricDataInput
	putMetricAlarmCalls []*cloudwatch.PutMetricAlarmInput
}

var _ cloudWatchAPI = (*mockCloudWatch)(nil)

func (m *mockCloudWatch) PutMetricData(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
	m.mu.Lock()
	m.putMetricDataCalls = append(m.putMetricDataCalls, params)
	m.mu.Unlock()

	if m.PutMetricDataFn != nil {
		return m.PutMetricDataFn(ctx, params, optFns...)
	}
	return &cloudwatch.PutMetricDataOutput{}, nil
}

func (m *mockCloudWatch) PutMetricAlarm(ctx context.Context, params *cloudwatch.PutMetricAlarmInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricAlarmOutput, error) {
	m.mu.Lock()
	m.putMetricAlarmCalls = append(m.putMetricAlarmCalls, params)
	m.mu.Unlock()

	if m.PutMetricAlarmFn != nil {
		return m.PutMetricAlarmFn(ctx, params, optFns...)
	}
	return &cloudwatch.PutMetricAlarmOutput{}, nil
}

func (m *mockCloudWatch) metricDataCalls() []*cloudwatch.PutMetricDataInput {
	m.mu.Lock()
	defer m.mu.Unlock()
	dst := make([]*cloudwatch.PutMetricDataInput, len(m.putMetricDataCalls))
	copy(dst, m.putMetricDataCalls)
	return dst
}

func (m *mockCloudWatch) metricAlarmCalls() []*cloudwatch.PutMetricAlarmInput {
	m.mu.Lock()
	defer m.mu.Unlock()
	dst := make([]*cloudwatch.PutMetricAlarmInput, len(m.putMetricAlarmCalls))
	copy(dst, m.putMetricAlarmCalls)
	return dst
}
