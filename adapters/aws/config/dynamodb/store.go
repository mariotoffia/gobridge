package dynamodb

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Validate performs structural validation and returns advisory warnings.
func (l *Loader) Validate(ctx context.Context, cfg *ports.BridgeConfig) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return config.ValidateWithWarnings(cfg)
}

// Merge applies an overlay without mutating either input.
func (l *Loader) Merge(ctx context.Context, base, overlay *ports.BridgeConfig) (*ports.BridgeConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return config.DefaultMerge(base, overlay)
}

// Save reads the current committed version, then conditionally writes version+1.
// A writer racing this read returns shared.ErrVersionMismatch. Callers editing
// a previously loaded config should use SaveIfVersion to protect that baseline.
func (l *Loader) Save(ctx context.Context, cfg *ports.BridgeConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg == nil {
		return shared.ErrInvalidConfig.WithMessage("dynamodb config save: config must not be nil")
	}
	current, err := l.currentVersion(ctx)
	if err != nil {
		return err
	}
	return l.SaveIfVersion(ctx, cfg, int(current))
}

// SaveIfVersion writes only when the stored version matches expectedVersion.
// An absent or versionless row can be created or adopted only at version zero.
// On success both the document and cfg.Version carry expectedVersion+1.
func (l *Loader) SaveIfVersion(ctx context.Context, cfg *ports.BridgeConfig, expectedVersion int) error {
	_, err := l.save(ctx, cfg, expectedVersion, false)
	return err
}

// CreateIfAbsent publishes version 1 only if the entire target item is absent.
// Unlike version-zero CAS it never adopts an existing versionless item.
func (l *Loader) CreateIfAbsent(ctx context.Context, cfg *ports.BridgeConfig) (bool, error) {
	return l.save(ctx, cfg, 0, true)
}

func (l *Loader) save(ctx context.Context, cfg *ports.BridgeConfig, expectedVersion int, createOnly bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if cfg == nil {
		return false, shared.ErrInvalidConfig.WithMessage("dynamodb config save: config must not be nil")
	}
	if expectedVersion < 0 || expectedVersion == math.MaxInt {
		return false, shared.ErrInvalidConfig.WithMessage("dynamodb config save: expected version cannot be incremented")
	}
	next := *cfg
	next.Version = expectedVersion + 1
	data, err := parser.MarshalBridgeConfigJSON(&next)
	if err != nil {
		return false, fmt.Errorf("dynamodb config save: marshal: %w", err)
	}
	if len(data) > maxConfigItemBytes {
		return false, fmt.Errorf("dynamodb config save: serialized config is %d bytes, which exceeds the %d-byte per-item limit (DynamoDB caps a single item at 400 KB); reduce the configuration size", len(data), maxConfigItemBytes)
	}
	if err := l.session.putConfigItem(ctx, l.pk(), data, int64(next.Version), int64(expectedVersion), createOnly); err != nil {
		if isConditionFailed(err) {
			if createOnly {
				return false, nil
			}
			return false, shared.ErrVersionMismatch.
				WithMessage("dynamodb config save: concurrent update detected; reload and retry").
				With("expectedVersion", expectedVersion)
		}
		return false, err
	}
	cfg.Version = next.Version
	l.mu.Lock()
	l.lastVersion = int64(next.Version)
	l.mu.Unlock()
	return true, nil
}

// EnsureTable creates the DynamoDB table if it does not already exist.
// When the loader runs in ModeStreams the table is provisioned with a
// KEYS_ONLY stream so self-provisioned deployments actually get the
// streams-based Watch they configured instead of silently degrading to
// poll mode. Intended for test setup and local development.
func (l *Loader) EnsureTable(ctx context.Context) error {
	if err := l.session.ensureTable(ctx, l.mode == ModeStreams); err != nil {
		return err
	}
	return l.session.waitTableExists(ctx, 30*time.Second)
}

var (
	_ ports.ConfigStore            = (*Loader)(nil)
	_ ports.ConditionalConfigStore = (*Loader)(nil)
	_ ports.ConfigInitializer      = (*Loader)(nil)
)
