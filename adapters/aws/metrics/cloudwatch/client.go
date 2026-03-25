package cloudwatch

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
)

// cloudWatchAPI is the subset of the CloudWatch SDK client used by this
// package. The real *cloudwatch.Client satisfies this interface. Tests
// supply a mock.
type cloudWatchAPI interface {
	PutMetricData(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error)
	PutMetricAlarm(ctx context.Context, params *cloudwatch.PutMetricAlarmInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricAlarmOutput, error)
}

var _ cloudWatchAPI = (*cloudwatch.Client)(nil)
