// Package observability provides shared signal-emission helpers used by
// the runtime, adapters, and processors to produce structured logs,
// metrics, and traces in a consistent shape.
//
// Responsibility:
//   - propagate request-scoped attributes (route ID, envelope ID,
//     correlation ID) through context so log/metric/trace emission can
//     stamp them uniformly without callers threading parameters
//   - own the slog handler that injects those attributes into every
//     record, keeping the runtime free of attribute-plumbing code
//
// Key types:
//   - context helpers that attach and read observability attributes
//   - SlogHandler: the slog.Handler implementation that materializes
//     contextual attributes onto log records
//
// Dependencies: stdlib only (context, log/slog). The package is imported
// by runtime and by adapters that emit structured logs; it depends on
// none of them.
package observability
