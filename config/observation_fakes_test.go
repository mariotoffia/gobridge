package config

import (
	"context"

	"github.com/mariotoffia/gobridge/ports"
)

type observationLoader struct {
	stubLoader
	observations chan ports.ConfigObservation
}

type restartingObserver struct {
	stubLoader
	channels chan chan ports.ConfigObservation
}

func (s *restartingObserver) Observe(ctx context.Context) (<-chan ports.ConfigObservation, error) {
	select {
	case ch := <-s.channels:
		return ch, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *observationLoader) Observe(context.Context) (<-chan ports.ConfigObservation, error) {
	return s.observations, nil
}

var _ ports.ConfigObserver = (*observationLoader)(nil)
