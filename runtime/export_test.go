package runtime

import "time"

// ComputeBatchDeadlineForTest exposes the internal computeBatchDeadline
// helper to external test packages. It is only compiled in _test.go
// builds.
func ComputeBatchDeadlineForTest(batchCount int, cfg OutboxDrainerConfig) time.Duration {
	return computeBatchDeadline(batchCount, cfg)
}
