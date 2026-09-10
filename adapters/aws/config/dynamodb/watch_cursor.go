package dynamodb

import (
	"bytes"
	"context"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Load retrieves the current BridgeConfig from DynamoDB. Its first successful
// read, including an absent item, establishes the initial watch baseline. Later
// admin reads must not advance the watcher past config it has not delivered.
func (l *Loader) Load(ctx context.Context) (*ports.BridgeConfig, error) {
	cfg, _, err := l.load(ctx)
	return cfg, err
}

func (l *Loader) load(ctx context.Context) (*ports.BridgeConfig, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	rawData, version, found, err := l.session.getConfigItem(ctx, l.pk())
	if err != nil {
		return nil, false, err
	}
	if !found {
		l.recordLoadedVersion(0)
		return nil, true, shared.ErrNotFound.WithMessage("config not found for bridge " + l.bridgeID)
	}

	cfg, err := parser.Parse(bytes.NewReader([]byte(rawData)), parser.FormatJSON, l.registry)
	if err != nil {
		return nil, false, shared.ErrInvalidConfig.WithMessage("dynamodb config load: parse").Wrap(err)
	}

	// The row version is the CAS authority, including for externally seeded
	// documents whose JSON version is absent or differs from the row.
	cfg.Version = int(version)
	l.recordLoadedVersion(version)

	return cfg, false, nil
}

// recordLoadedVersion distinguishes an established empty baseline from a loader
// that has never loaded config. Only the first Load seeds an unset baseline:
// an admin read during startup or an iterator gap is not a watcher delivery.
func (l *Loader) recordLoadedVersion(version int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastVersion = version
	if !l.watchHasBaseline {
		l.watchVersion = version
		l.watchHasBaseline = true
	}
}

// beginWatchCursor freezes the initial baseline before any asynchronous work.
// Save-before-Watch without a Load retains the standalone loader convention:
// the saved config is the initial baseline, unless a Load already established it.
// Calling this again on a poll/streams handoff keeps the last delivered version.
func (l *Loader) beginWatchCursor() (int64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.watchStarted {
		l.watchStarted = true
		if !l.watchHasBaseline && l.lastVersion > 0 {
			l.watchVersion = l.lastVersion
			l.watchHasBaseline = true
		}
	}
	return l.watchVersion, l.watchHasBaseline
}

func (l *Loader) watchCursor() (int64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.watchVersion, l.watchHasBaseline
}

// recordWatchDelivery acknowledges only config enqueued on the watch channel.
// This cursor survives stream iterator gaps and poll/streams handoffs without
// being overwritten by ordinary ConfigStore Load, Save or SaveIfVersion calls.
func (l *Loader) recordWatchDelivery(version int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.watchVersion = version
	l.watchHasBaseline = true
}
