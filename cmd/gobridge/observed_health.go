package main

import (
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/httpapi"
	"github.com/mariotoffia/gobridge/ports"
)

func observedConfigHealth(session *configSession, manager *config.Manager, control *ports.HTTPConfig, activated bool, sourceError uint32) httpapi.ConfigWatchHealth {
	status := httpapi.ConfigWatchHealth{ReconfigurePending: manager.ReconfigurePending(), Reason: "awaiting configuration"}
	if session != nil {
		status = configWatchHealth(session.sup, manager, control)
	}
	if sourceError != 0 {
		status.Degraded, status.Reason = true, "configuration repository or activation failed"
	}
	status.StartupPending = !activated && sourceError != 2
	if session != nil && session.sup.Terminal() {
		status.StartupPending = false
	}
	for _, err := range manager.WatchErrors() {
		if !httpapi.ConfigStartupPending(err) {
			status.StartupPending = false
		}
	}
	if err := manager.LastApplyError(); err != nil {
		status.LastApplyError, status.Degraded = err.Error(), true
		if !httpapi.ConfigStartupPending(err) {
			status.StartupPending = false
		}
	}
	return status
}
