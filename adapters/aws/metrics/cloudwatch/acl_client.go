package cloudwatch

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
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

// newCloudWatchClient resolves AWS configuration and constructs the
// concrete CloudWatch SDK client wrapped behind the cloudWatchAPI seam.
//
//nolint:ireturn // cloudWatchAPI is an adapter-internal mock seam (category 5).
func newCloudWatchClient(ctx context.Context, cfg Config) (cloudWatchAPI, error) {
	awsCfg, err := loadAWSConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return cloudwatch.NewFromConfig(awsCfg), nil
}

// loadAWSConfig builds an aws.Config from this adapter's Config, honouring
// Region and Endpoint overrides and falling back to the SDK default chain.
func loadAWSConfig(ctx context.Context, cfg Config) (aws.Config, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("cloudwatch: load default aws config: %w", err)
	}
	if cfg.Endpoint != "" {
		awsCfg.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	return awsCfg, nil
}
