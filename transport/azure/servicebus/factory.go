package servicebus

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// SourceFactory creates Azure Service Bus sources.
type SourceFactory struct{}

// Ensure SourceFactory implements types.SourceFactory
var _ types.SourceFactory = (*SourceFactory)(nil)

// NewSourceFactory creates a new Service Bus source factory.
func NewSourceFactory() *SourceFactory {
	return &SourceFactory{}
}

// SupportedTransports returns the transport types this factory supports.
func (f *SourceFactory) SupportedTransports() []types.TransportType {
	return []types.TransportType{TransportType}
}

// CreateSource creates a new Service Bus source.
func (f *SourceFactory) CreateSource(ctx context.Context, config types.SourceConfig) (types.Source, error) {
	sbConfig, ok := config.(*SourceConfigImpl)
	if !ok {
		return nil, fmt.Errorf("invalid config type: expected *servicebus.SourceConfigImpl, got %T", config)
	}

	return NewSource(sbConfig)
}

// TargetFactory creates Azure Service Bus targets.
type TargetFactory struct{}

// Ensure TargetFactory implements types.TargetFactory
var _ types.TargetFactory = (*TargetFactory)(nil)

// NewTargetFactory creates a new Service Bus target factory.
func NewTargetFactory() *TargetFactory {
	return &TargetFactory{}
}

// SupportedTransports returns the transport types this factory supports.
func (f *TargetFactory) SupportedTransports() []types.TransportType {
	return []types.TransportType{TransportType}
}

// CreateTarget creates a new Service Bus target.
func (f *TargetFactory) CreateTarget(ctx context.Context, config types.TargetConfig) (types.Target, error) {
	sbConfig, ok := config.(*TargetConfigImpl)
	if !ok {
		return nil, fmt.Errorf("invalid config type: expected *servicebus.TargetConfigImpl, got %T", config)
	}

	return NewTarget(sbConfig)
}

