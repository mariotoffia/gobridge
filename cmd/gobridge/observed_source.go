package main

import (
	"context"

	fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
	"github.com/mariotoffia/gobridge/ports"
)

type commandObservation struct {
	ports.ConfigObservation
	generation uint64
}

// One worker owns source establishment and observation. An uncancellable file
// read cannot block control-plane handlers or cause repeated read goroutines.
func observeCommandConfig(ctx context.Context, source ports.Loader, watcher *fileconfig.Watcher, observer ports.ConfigObserver, withdraw func(), generation func() uint64, report func(error)) <-chan commandObservation {
	observations := make(chan commandObservation)
	go func() {
		defer close(observations)
		// Observe takes its own authoritative initial snapshot after this policy read.
		if boot, err := source.Load(ctx); err == nil {
			fileconfig.WithWatchConfig(boot.ConfigWatch)(watcher)
		}
		ch, err := observer.Observe(ctx)
		if err != nil {
			report(err)
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				if ev.Kind == ports.ConfigMissing {
					withdraw()
				}
				select {
				case observations <- commandObservation{ev, generation()}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return observations
}
