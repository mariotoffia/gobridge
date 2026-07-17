package ports

import (
	"context"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// ProcessorFunc is the continuation function passed to Processor.Process.
// Calling it invokes the remainder of the chain (the next processor, or the
// terminal dispatch step) with the given envelope.
type ProcessorFunc func(ctx context.Context, env *messaging.Envelope) error

// Processor is a single element in the message processing chain. Processors
// handle validation, filtering, transformation, enrichment, and routing
// decisions. They must not own transport lifecycle.
//
// Processors compose as an ONION: Process receives the envelope and a `next`
// continuation, does its pre-work, optionally calls next to descend into the
// rest of the chain, then optionally does post-work as the call unwinds.
//
// The following rules are NORMATIVE. PLUGIN.md is the tutorial; this godoc is
// the contract. A processor that breaks these rules can corrupt the envelope
// or crash the process.
//
//   - Mutate BEFORE next, never after. A processor MUST perform all envelope
//     mutation (env.Headers, body, metadata) BEFORE it calls next(ctx, env).
//     Once next has been called the envelope MUST be treated as read-only for
//     the remainder of this Process invocation, INCLUDING any post-next
//     unwind work. Writing to the envelope after next has been entered races
//     the settlement path and any concurrently-unwinding outer frame that
//     shares the same Headers map — a concurrent map write, which crashes the
//     process. Do post-work that only reads the envelope, or returns an error.
//
//   - Call next AT MOST ONCE. Each Process invocation MUST call next zero or
//     one time. Call it once to pass through (descend into the chain); do NOT
//     call it to short-circuit (return an error, or nil, without calling
//     next). A processor MUST NOT call next more than once: a second call
//     re-runs the downstream chain and terminal dispatch against the same
//     delivery, double-processing and double-settling it.
//
//   - Do NOT retain the envelope past return. The envelope and its Headers map
//     are owned by this chain frame for the duration of the Process call only.
//     A processor MUST NOT retain a reference to env (or env.Headers) beyond
//     the return of its own Process call, and MUST NOT hand it to a goroutine
//     that outlives the call. After Process returns, the runtime may settle,
//     recycle, or concurrently mutate the envelope; a retained reference
//     reintroduces the same data race as post-next mutation.
//
//   - Honour context cancellation. When the passed ctx is cancelled (e.g. the
//     per-processor ProcessorTimeout elapsed), a processor MUST stop work and
//     return promptly. The runtime cancels an over-budget processor's ctx to
//     unwind it and refuses to merge the result; a processor that ignores
//     cancellation is abandoned as a leaked goroutine still holding this
//     frame's envelope — the exact condition that turns rule 1's post-next
//     write into a live race.
//
//   - Error-class semantics select disposition. The error a processor returns
//     drives what happens to the message:
//     return nil to accept (the chain, and ultimately the delivery, succeeds);
//     return shared.ErrMessageFiltered to FILTER — a deliberate drop, not a
//     failure. The delivery is acked; by default no DLQ record is written. A
//     route records it to the DLQ only when OnFiltered is FilteredDLQ and a DLQ
//     store exists;
//     return any other error to short-circuit — its ErrorClass selects the
//     disposition (ErrorTransient → retry, ErrorPermanent → DLQ, ErrorRejected
//     → drop without DLQ, ErrorExpired → expired-action). Prefer returning a
//     *shared.BridgeError (or an error that wraps one) so the class is
//     explicit; the runtime classifies an unclassified error conservatively.
//     A processor that overruns its budget is failed by the runtime with
//     shared.ErrProcessorTimeout (transient) and a panicking one with
//     shared.ErrProcessorPanic (permanent) — processors do not return these
//     themselves.
type Processor interface {
	Name() string
	Process(ctx context.Context, env *messaging.Envelope, next ProcessorFunc) error
}
