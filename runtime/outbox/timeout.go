package outbox

import "time"

// ComputeBatchDeadline returns the wall-clock timeout to apply to a
// single drain batch, given the number of records claimed.
//
// When neither PerRecordDrainTimeout nor MaxDrainTimeout is set, the
// legacy fixed DrainTimeout is returned to preserve backward-compatible
// behavior. Otherwise the timeout scales with batch size:
//
//	scaled = batchCount * PerRecordDrainTimeout
//	deadline = min(scaled, MaxDrainTimeout)
//
// We use min (not max) so that MaxDrainTimeout acts as a true ceiling —
// large batches cannot produce unbounded timeouts. A PerRecord floor
// still applies so that a batchCount of 0 (which should not occur in
// practice because drainBatch returns early on empty claims) does not
// collapse the deadline to zero.
func ComputeBatchDeadline(batchCount int, cfg Config) time.Duration {
	// Backward-compat path: if new fields unset, honor legacy DrainTimeout.
	if cfg.PerRecordDrainTimeout == 0 && cfg.MaxDrainTimeout == 0 {
		return cfg.DrainTimeout
	}
	per := cfg.PerRecordDrainTimeout
	if per == 0 {
		per = defaultPerRecordDrainTimeout
	}
	maxCap := cfg.MaxDrainTimeout
	if maxCap == 0 {
		maxCap = defaultMaxDrainTimeout
	}
	scaled := time.Duration(batchCount) * per
	if scaled <= 0 || scaled > maxCap {
		return maxCap
	}
	if scaled < per {
		return per
	}
	return scaled
}

// batchDeadline computes the batch deadline for this drainer instance
// based on the currently configured values and the provided batch size.
// It exists so the drain loop can derive a per-batch deadline without
// holding a copy of Config.
func (d *Drainer) batchDeadline(batchCount int) time.Duration {
	if !d.useScaledTimeout {
		return d.drainTimeout
	}
	return ComputeBatchDeadline(batchCount, Config{
		PerRecordDrainTimeout: d.perRecordDrainTimeout,
		MaxDrainTimeout:       d.maxDrainTimeout,
		DrainTimeout:          d.drainTimeout,
	})
}

// batchTimeout returns the wall-clock budget for a whole drain batch's
// send+complete work (finding 10). It must never undercut a single record's
// SendTimeout + Complete margin, and it scales with the sequential send depth
// the batch needs: the number of concurrency waves (recordCount/maxConcurrency)
// OR the longest ordering group (whose records send strictly sequentially),
// whichever is larger. The configured DrainTimeout/MaxDrainTimeout ceiling may
// only RAISE this budget — it can no longer cut it below one full send, which
// was the silent SendTimeout cap the previous min()-with-ceiling introduced.
func (d *Drainer) batchTimeout(recordCount int, groups []orderingGroup) time.Duration {
	perSend := d.policy.SendTimeout + d.completeBudget()
	if perSend <= 0 {
		perSend = d.batchTimeoutFloor
	}

	conc := d.maxConcurrency
	if conc <= 0 {
		conc = 1
	}
	waves := (recordCount + conc - 1) / conc

	longest := 0
	for _, g := range groups {
		if len(g) > longest {
			longest = len(g)
		}
	}

	depth := waves
	if longest > depth {
		depth = longest
	}
	if depth < 1 {
		depth = 1
	}

	budget := time.Duration(depth) * perSend
	if budget < perSend {
		budget = perSend
	}
	if budget < d.batchTimeoutFloor {
		budget = d.batchTimeoutFloor
	}
	// The configured ceiling may only raise the budget; never undercut a send.
	if ceiling := d.batchDeadline(recordCount); ceiling > budget {
		budget = ceiling
	}
	return budget
}
