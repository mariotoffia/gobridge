package config

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Initialize creates an absent target from a declarative source. Writer-role
// authorization and the first-activation latch belong to the caller. Only
// shared.ErrNotFound denotes document absence; storage/read faults never seed.
// Admission receives an isolated snapshot: runtime credential resolution or
// other admission mutations cannot enter the document being published.
// Success means the authoritative target was reloaded and admitted, not that
// its data plane is running. Existing targets never access source.
func Initialize(ctx context.Context, target ports.ConfigStore, source ports.Loader, admit func(context.Context, *ports.BridgeConfig) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if target == nil {
		return shared.ErrInvalidConfig.WithMessage("config initialization requires a target")
	}
	current, err := target.Load(ctx)
	if err == nil {
		return admitInitial(ctx, target, current, admit)
	}
	if !initialMissing(err) || source == nil {
		return fmt.Errorf("config initialization: load target: %w", err)
	}
	initializer, ok := target.(ports.ConfigInitializer)
	if !ok {
		return shared.ErrNotSupported.WithMessage("config target does not support strict initialization")
	}
	seed, err := source.Load(ctx)
	if err != nil {
		return fmt.Errorf("config initialization: load source: %w", err)
	}
	candidate, err := initialSnapshot(seed)
	if err != nil {
		return err
	}
	candidate.Version = 1 // source revision metadata has no target authority.
	if err := admitInitial(ctx, target, candidate, admit); err != nil {
		return err
	}
	_, writeErr := initializer.CreateIfAbsent(ctx, candidate)
	winner, loadErr := target.Load(ctx)
	if loadErr == nil {
		loadErr = admitInitial(ctx, target, winner, admit)
	}
	if writeErr != nil {
		if loadErr == nil && ambiguousInitialWrite(writeErr) {
			return nil
		}
		return fmt.Errorf("config initialization: publish/reconcile: %w", errors.Join(writeErr, loadErr))
	}
	if loadErr != nil {
		return fmt.Errorf("config initialization: reload target: %w", loadErr)
	}
	return nil
}

func admitInitial(ctx context.Context, target ports.ConfigStore, cfg *ports.BridgeConfig, admit func(context.Context, *ports.BridgeConfig) error) error {
	snapshot, err := initialSnapshot(cfg)
	if err != nil {
		return err
	}
	if _, err := target.Validate(ctx, snapshot); err != nil {
		return fmt.Errorf("config initialization: validate: %w", err)
	}
	if err := forEachPluginConfig(snapshot, func(plugin ports.PluginConfig) error {
		if plugin == nil {
			return nil
		}
		return plugin.Validate()
	}); err != nil {
		return fmt.Errorf("config initialization: validate plugin: %w", err)
	}
	if admit != nil {
		if err := admit(ctx, snapshot); err != nil {
			return fmt.Errorf("config initialization: admit: %w", err)
		}
	}
	return nil
}

func ambiguousInitialWrite(err error) bool {
	var classified *shared.BridgeError
	if errors.As(err, &classified) {
		return classified.Code == shared.ErrCodeTimeout || classified.Code == shared.ErrCodeUnavailable ||
			classified.Code == shared.ErrCodeConnectionLost
	}
	var networkError net.Error
	return errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &networkError) && networkError.Timeout())
}

// Inspect the outer classification: a decoder can legitimately wrap a missing
// reference inside INVALID_CONFIG. That is not an absent target document.
func initialMissing(err error) bool {
	var classified *shared.BridgeError
	return errors.As(err, &classified) && classified.Code == shared.ErrCodeNotFound
}
