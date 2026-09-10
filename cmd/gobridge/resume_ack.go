package main

import (
	"context"
	"fmt"

	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
)

func (s *configSession) recordObservation(cfg *ports.BridgeConfig, generation uint64) {
	s.observed.Store(&commandObservation{ConfigObservation: ports.ConfigObservation{Kind: ports.ConfigPresent, Config: cfg}, generation: generation})
}

func (c observedController) resume(ctx context.Context) error {
	if c.absent.Load() {
		return fmt.Errorf("configuration absent")
	}
	session := c.current.Load()
	if session == nil {
		return fmt.Errorf("awaiting configuration")
	}
	observed := session.observed.Load()
	generation := c.generation.Load()
	if observed == nil || observed.generation != generation {
		return fmt.Errorf("configuration observation is no longer current")
	}
	if err := session.sup.StartBridge(ctx); err != nil {
		return err
	}
	if c.absent.Load() || c.current.Load() != session || c.generation.Load() != generation {
		return fmt.Errorf("configuration generation changed during resume")
	}
	session.acknowledgeResume(c.manager, observed.Config)
	return nil
}

// A paused change is only desired until StartBridge succeeds. Echo the exact
// captured observation, not the Supervisor's copy. A newer emission is rejected
// by Manager's identity guard; a newer applied config owns its own swap ack.
func (s *configSession) acknowledgeResume(manager *config.Manager, observed *ports.BridgeConfig) {
	rt := s.sup.Runtime()
	if rt == nil || !rt.IsRunning() {
		return
	}
	expected, expectedErr := bridge.ConfigArtifactDigest(observed)
	running, runningErr := bridge.ConfigArtifactDigest(s.sup.Config())
	if expectedErr == nil && runningErr == nil && expected == running {
		manager.NotifyApplyResult(observed, nil)
	}
}
