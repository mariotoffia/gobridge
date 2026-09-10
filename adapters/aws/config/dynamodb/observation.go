package dynamodb

import (
	"context"
	"crypto/sha256"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

type configObservation struct {
	out    chan ports.ConfigObservation
	kind   ports.ConfigObservationKind
	digest [sha256.Size]byte
}

// Observe shares Watch's polling/streams implementation, but reports complete
// snapshots, absence and failures without latest-wins coalescing.
func (l *Loader) Observe(ctx context.Context) (<-chan ports.ConfigObservation, error) {
	out := make(chan ports.ConfigObservation, 1)
	_, err := l.watch(ctx, &configObservation{out: out})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (l *Loader) watch(ctx context.Context, observation *configObservation) (<-chan *ports.BridgeConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	if l.watching {
		l.mu.Unlock()
		return nil, shared.ErrAlreadyExists.WithMessage("dynamodb config watcher already running")
	}
	l.watching, l.observation = true, observation
	l.mu.Unlock()
	l.beginWatchCursor()
	ch := make(chan *ports.BridgeConfig, 1)
	if observation != nil {
		l.observeCurrent(ctx)
	}
	if l.mode == ModeStreams {
		arn, reason := l.resolveStreamArn(ctx)
		go func() {
			defer l.finishWatch()
			if reason == "" {
				l.streamLoop(ctx, ch, arn)
			} else {
				l.observeResult(ctx, nil, shared.ErrUnavailable.WithMessage(reason))
				if l.logger != nil {
					l.logger.Warn("dynamodb config loader: streams unavailable; polling", "reason", reason)
				}
				l.superviseStreamReacquire(ctx, ch)
			}
		}()
	} else {
		ticker := l.clk.NewTicker(l.pollInterval)
		go func() {
			defer l.finishWatch()
			l.pollLoop(ctx, ch, ticker)
		}()
	}
	return ch, nil
}

func (l *Loader) finishWatch() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.watching = false
	if l.observation != nil {
		close(l.observation.out)
		l.observation = nil
	}
}

func (l *Loader) observeCurrent(ctx context.Context) {
	cfg, missing, err := l.load(ctx) // full strong read: zero does not prove absence.
	kind := ports.ConfigPresent
	if err != nil {
		kind = ports.ConfigReadError
	}
	if missing {
		kind = ports.ConfigMissing
	}
	l.observe(ctx, kind, cfg, err)
}

func (l *Loader) observeResult(ctx context.Context, cfg *ports.BridgeConfig, err error) {
	kind := ports.ConfigPresent
	if err != nil {
		kind = ports.ConfigReadError
	}
	l.observe(ctx, kind, cfg, err)
}

func (l *Loader) observe(ctx context.Context, kind ports.ConfigObservationKind, cfg *ports.BridgeConfig, err error) {
	o := l.observation
	if o == nil {
		return
	}
	var data []byte
	if err == nil {
		data, err = parser.MarshalBridgeConfigJSON(cfg)
	}
	if err != nil {
		cfg = nil
		if kind != ports.ConfigMissing {
			kind = ports.ConfigReadError
		}
		data = []byte(err.Error())
	}
	if kind == ports.ConfigMissing {
		data = nil
	}
	digest := sha256.Sum256(data)
	if kind == o.kind && digest == o.digest {
		return
	}
	select {
	case <-ctx.Done():
	case o.out <- ports.ConfigObservation{Kind: kind, Config: cfg, Err: err, Sequence: l.observationSequence + 1}:
		l.observationSequence++
		o.kind, o.digest = kind, digest
	}
}

var _ ports.ConfigObserver = (*Loader)(nil)
