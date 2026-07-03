package validate

func (cfg *BridgeConfig) isClustered() bool {
	return cfg.DeploymentMode == "clustered"
}

// hasProcessLocalLeaseStore reports whether a lease store is configured but is
// process-local (memory, sqlite) and therefore provides NO cross-instance
// coordination.
func (cfg *BridgeConfig) hasProcessLocalLeaseStore() bool {
	return cfg.HasLeaseStore && !cfg.LeaseStoreDistributed
}

func validateStoreBackends(cfg *BridgeConfig, errs *ValidationErrors) {
	// A process-local lease store provides no cross-instance coordination, so
	// running more than one instance against it silently split-brains: each
	// instance wins its OWN local lease and both believe they are the exclusive
	// owner (finding 11).
	//
	// The split-brain hazard is proven whenever multi-instance intent is
	// present. Two independent signals express that intent:
	//
	//   1. deployment_mode: clustered  (explicit; covered below).
	//   2. cluster endpoints configured (bridge.cluster.endpoints): the operator
	//      has told the bridge how to reach peer instances, which only makes
	//      sense with more than one instance.
	//
	// Signal (2) is NOT yet threaded into the validator's BridgeConfig (the
	// struct lives in the read-only validate/config.go and carries no
	// cluster-endpoints field). Wiring it requires a cross-module change — see
	// the finding-11 DEFERRED note in the agent report. Until then only the
	// explicit clustered mode trips the guard; an unset deployment_mode keeps
	// its historical implicit single-instance assumption so existing
	// standalone/dev configs continue to validate.
	if cfg.isClustered() {
		if cfg.hasProcessLocalLeaseStore() {
			*errs = append(*errs, ValidationError{
				Rule:    "store_backend",
				Message: "clustered deployment requires a distributed LeaseStore; process-local stores (memory, sqlite) provide no cross-instance coordination",
			})
		}

		if cfg.HasOutboxStore && !cfg.OutboxStoreDistributed {
			*errs = append(*errs, ValidationError{
				Rule:    "store_backend",
				Message: "clustered deployment requires a distributed OutboxStore; process-local stores (memory, sqlite) provide no cross-instance coordination",
			})
		}

		if cfg.HasDLQStore && !cfg.DLQStoreDistributed {
			*errs = append(*errs, ValidationError{
				Rule:    "store_backend",
				Message: "clustered deployment requires a distributed DLQStore; process-local stores (memory, sqlite) provide no cross-instance coordination",
			})
		}
	}
}
