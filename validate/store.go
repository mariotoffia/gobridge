package validate

func (cfg *BridgeConfig) isClustered() bool {
	return cfg.DeploymentMode == "clustered"
}

func validateStoreBackends(cfg *BridgeConfig, errs *ValidationErrors) {
	if !cfg.isClustered() {
		return
	}

	if cfg.HasLeaseStore && !cfg.LeaseStoreDistributed {
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
