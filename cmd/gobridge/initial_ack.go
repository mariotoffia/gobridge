package main

import (
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
)

// acknowledgeInitial echoes the exact observation snapshot, never the loader's
// reusable pointer or the Supervisor's frozen copy. The command loop owns this
// one-time acknowledgement; later applies report through pipeline.onSwap.
func (s *configSession) acknowledgeInitial(manager *config.Manager) {
	if s.initialAcknowledged || s.initial == nil {
		return
	}
	rt := s.sup.Runtime()
	if rt == nil || !rt.IsRunning() {
		return
	}
	observed, observedErr := bridge.ConfigArtifactDigest(s.initial)
	running, runningErr := bridge.ConfigArtifactDigest(s.sup.Config())
	if observedErr != nil || runningErr != nil {
		return
	}
	s.initialAcknowledged = true
	// A newer apply may have overtaken the initial runtime before this check.
	// Its pipeline acknowledgement owns running state; never adopt a cached copy.
	if observed == running {
		manager.NotifyApplyResult(s.initial, nil)
	}
}
