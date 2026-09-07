//go:build gobridge_aws || gobridge_all

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	sqsadapter "github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

//nolint:gochecknoinits // Records build-tag metadata only; decoder and factory registration remain explicit.
func init() { compiledFamilies = append(compiledFamilies, "aws") }

func registerAWSDecoders(reg *ports.Registry) error {
	return errors.Join(sqsadapter.Register(reg), awsstore.Register(reg))
}

func wireAWSFactories(ctx context.Context, sup *bridge.Supervisor, logger *slog.Logger, metrics ports.MetricsExporter) error {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS configuration: %w", err)
	}
	var sqsFactory *sqsadapter.Factory
	if metrics != nil {
		sqsFactory = sqsadapter.NewFactory(logger, metrics)
	} else {
		sqsFactory = sqsadapter.NewFactory(logger)
	}
	sup.RegisterTransport("sqs", sqsFactory)
	sup.RegisterTransport("aws.sqs", sqsFactory)
	sup.RegisterStoreFactory("dynamodb",
		awsstore.NewDynamoDBStoreFactory(dynamodb.NewFromConfig(awsCfg)))
	return nil
}

func seedAWSStores(ctx context.Context, b *bridge.Builder) error {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS configuration: %w", err)
	}
	b.RegisterStoreFactory("dynamodb",
		awsstore.NewDynamoDBStoreFactory(dynamodb.NewFromConfig(awsCfg)))
	return nil
}
