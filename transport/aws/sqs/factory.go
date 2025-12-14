package sqs

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// SourceFactory creates SQS sources.
type SourceFactory struct{}

// Ensure SourceFactory implements types.SourceFactory
var _ types.SourceFactory = (*SourceFactory)(nil)

// NewSourceFactory creates a new SQS source factory.
func NewSourceFactory() *SourceFactory {
	return &SourceFactory{}
}

// SupportedTransports returns the transport types this factory supports.
func (f *SourceFactory) SupportedTransports() []types.TransportType {
	return []types.TransportType{TransportType}
}

// CreateSource creates a new SQS source.
func (f *SourceFactory) CreateSource(ctx context.Context, config types.SourceConfig) (types.Source, error) {
	sqsConfig, ok := config.(*SourceConfigImpl)
	if !ok {
		return nil, fmt.Errorf("invalid config type: expected *sqs.SourceConfigImpl, got %T", config)
	}

	return NewSource(sqsConfig)
}

// TargetFactory creates SQS targets.
type TargetFactory struct{}

// Ensure TargetFactory implements types.TargetFactory
var _ types.TargetFactory = (*TargetFactory)(nil)

// NewTargetFactory creates a new SQS target factory.
func NewTargetFactory() *TargetFactory {
	return &TargetFactory{}
}

// SupportedTransports returns the transport types this factory supports.
func (f *TargetFactory) SupportedTransports() []types.TransportType {
	return []types.TransportType{TransportType}
}

// CreateTarget creates a new SQS target.
func (f *TargetFactory) CreateTarget(ctx context.Context, config types.TargetConfig) (types.Target, error) {
	sqsConfig, ok := config.(*TargetConfigImpl)
	if !ok {
		return nil, fmt.Errorf("invalid config type: expected *sqs.TargetConfigImpl, got %T", config)
	}

	return NewTarget(sqsConfig)
}

