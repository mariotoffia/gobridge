package sqlitedlq

import "github.com/mariotoffia/gobridge/domain/shared"

// MetricStoreUnhealthy counts fatal storage faults surfaced by this backend:
// a full disk, a corrupt database, a read-only database, or a file that is not
// a database (see isFatalStorageErr / errStoreFatal in acl_errors.go). Unlike
// the transient throttling/IO classes, a rising value here is a DISTINCT
// ALERTABLE signal: those faults are classified PERMANENT because retrying
// cannot clear them without operator action (free disk / restore the file).
// The DLQ is the last-resort failure sink, so a fault here silently swallowing
// evidence is exactly the condition an operator must see — this counter, not a
// transient-noise blend, is what an alert should watch. Emitted with a
// shared.TagKeyEntity="dlq" dimension.
//
// The name is adapter-owned (the same convention the sibling sqliteoutbox
// backend uses for backend-specific counters) rather than a
// domain/shared.Metric* constant, because it describes a SQLite-specific
// failure mode the runtime core has no concept of.
const MetricStoreUnhealthy = "SQLiteStoreUnhealthy"

// counterMeter is the minimal metrics surface this store needs: a single
// Counter method. It is declared locally (rather than importing
// ports.MetricsExporter) so this driven-adapter leaf keeps depending only on
// domain/shared per the architecture rules; ports.MetricsExporter and its
// Noop/Recording implementations satisfy it structurally, so the composition
// root can inject a real exporter through WithMetrics with no adapter glue.
type counterMeter interface {
	Counter(name string, value int64, tags ...shared.Tag)
}

// noopMeter is the default counterMeter: it discards every counter so the
// store never depends on a configured backend.
type noopMeter struct{}

func (noopMeter) Counter(string, int64, ...shared.Tag) {}
