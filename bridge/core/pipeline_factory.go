package core

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// PipelineFactoryImpl implements types.PipelineFactory.
// It creates pipelines by resolving sources and targets from either:
// 1. A shared Connection (when PipelineConfig.GetConnectionID() is set)
// 2. Independent SourceRegistry and TargetRegistry (traditional mode)
type PipelineFactoryImpl struct {
	sourceRegistry     types.SourceRegistry
	targetRegistry     types.TargetRegistry
	connectionRegistry types.ConnectionRegistry
	middlewareRegistry *MiddlewareRegistry
}

// Ensure PipelineFactoryImpl implements types.PipelineFactory
var _ types.PipelineFactory = (*PipelineFactoryImpl)(nil)

// PipelineFactoryOption configures a PipelineFactory.
type PipelineFactoryOption func(*PipelineFactoryImpl)

// PipelineFactoryWithSourceRegistry sets the source registry for independent source creation.
func PipelineFactoryWithSourceRegistry(registry types.SourceRegistry) PipelineFactoryOption {
	return func(f *PipelineFactoryImpl) {
		f.sourceRegistry = registry
	}
}

// PipelineFactoryWithTargetRegistry sets the target registry for independent target creation.
func PipelineFactoryWithTargetRegistry(registry types.TargetRegistry) PipelineFactoryOption {
	return func(f *PipelineFactoryImpl) {
		f.targetRegistry = registry
	}
}

// PipelineFactoryWithConnectionRegistry sets the connection registry for shared connection mode.
func PipelineFactoryWithConnectionRegistry(registry types.ConnectionRegistry) PipelineFactoryOption {
	return func(f *PipelineFactoryImpl) {
		f.connectionRegistry = registry
	}
}

// PipelineFactoryWithMiddlewareRegistry sets the middleware registry for pipeline middleware chain.
func PipelineFactoryWithMiddlewareRegistry(registry *MiddlewareRegistry) PipelineFactoryOption {
	return func(f *PipelineFactoryImpl) {
		f.middlewareRegistry = registry
	}
}

// NewPipelineFactory creates a new PipelineFactory.
func NewPipelineFactory(opts ...PipelineFactoryOption) *PipelineFactoryImpl {
	f := &PipelineFactoryImpl{}

	for _, opt := range opts {
		opt(f)
	}

	return f
}

// CreatePipeline creates a new Pipeline from the given configuration.
//
// If the configuration specifies a ConnectionID, the source and target are created
// from the shared Connection. Otherwise, they are created independently using the
// SourceRegistry and TargetRegistry.
func (f *PipelineFactoryImpl) CreatePipeline(ctx context.Context, config types.PipelineConfig) (types.Pipeline, error) {
	var source types.Source
	var target types.Target
	var err error

	connectionID := config.GetConnectionID()

	if connectionID != "" {
		// Shared connection mode
		source, target, err = f.createFromConnection(ctx, config, connectionID)
	} else {
		// Independent mode
		source, target, err = f.createIndependent(ctx, config)
	}

	if err != nil {
		return nil, err
	}

	// Build middleware chain
	var middlewares *types.MiddlewareChain
	if f.middlewareRegistry != nil && len(config.GetMiddlewareNames()) > 0 {
		middlewares, err = f.middlewareRegistry.CreateChain(config.GetMiddlewareNames()...)
		if err != nil {
			// Clean up created source/target
			if source != nil {
				_ = source.Close()
			}
			if target != nil {
				_ = target.Close()
			}
			return nil, fmt.Errorf("failed to build middleware chain: %w", err)
		}
	}

	// Create the pipeline
	pipeline := NewPipeline(
		config.GetID(),
		config.GetMode(),
		source,
		target,
		middlewares,
	)

	return pipeline, nil
}

// createFromConnection creates source and target from a shared Connection.
func (f *PipelineFactoryImpl) createFromConnection(
	ctx context.Context,
	config types.PipelineConfig,
	connectionID string,
) (types.Source, types.Target, error) {
	if f.connectionRegistry == nil {
		return nil, nil, fmt.Errorf("connection registry not configured but connectionId %q specified", connectionID)
	}

	// Get the connection
	conn, err := f.connectionRegistry.GetConnection(connectionID)
	if err != nil {
		return nil, nil, fmt.Errorf("connection %q not found: %w", connectionID, err)
	}

	var source types.Source
	var target types.Target

	// Create source from connection if source config provided
	if sourceConfig := config.GetSourceConfig(); sourceConfig != nil {
		sourceProvider := conn.SourceProvider()
		if sourceProvider == nil {
			return nil, nil, fmt.Errorf("connection %q does not support creating sources", connectionID)
		}

		source, err = sourceProvider.CreateSource(ctx, sourceConfig)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create source from connection %q: %w", connectionID, err)
		}
	}

	// Create target from connection if target config provided
	if targetConfig := config.GetTargetConfig(); targetConfig != nil {
		targetProvider := conn.TargetProvider()
		if targetProvider == nil {
			// Clean up source if created
			if source != nil {
				_ = source.Close()
			}
			return nil, nil, fmt.Errorf("connection %q does not support creating targets", connectionID)
		}

		target, err = targetProvider.CreateTarget(ctx, targetConfig)
		if err != nil {
			// Clean up source if created
			if source != nil {
				_ = source.Close()
			}
			return nil, nil, fmt.Errorf("failed to create target from connection %q: %w", connectionID, err)
		}
	}

	return source, target, nil
}

// createIndependent creates source and target using independent registries.
func (f *PipelineFactoryImpl) createIndependent(
	ctx context.Context,
	config types.PipelineConfig,
) (types.Source, types.Target, error) {
	var source types.Source
	var target types.Target
	var err error

	// Create source if config provided
	if sourceConfig := config.GetSourceConfig(); sourceConfig != nil {
		if f.sourceRegistry == nil {
			return nil, nil, fmt.Errorf("source registry not configured")
		}

		source, err = f.sourceRegistry.CreateSource(ctx, sourceConfig)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create source: %w", err)
		}
	}

	// Create target if config provided
	if targetConfig := config.GetTargetConfig(); targetConfig != nil {
		if f.targetRegistry == nil {
			// Clean up source if created
			if source != nil {
				_ = source.Close()
			}
			return nil, nil, fmt.Errorf("target registry not configured")
		}

		target, err = f.targetRegistry.CreateTarget(ctx, targetConfig)
		if err != nil {
			// Clean up source if created
			if source != nil {
				_ = source.Close()
			}
			return nil, nil, fmt.Errorf("failed to create target: %w", err)
		}
	}

	return source, target, nil
}
