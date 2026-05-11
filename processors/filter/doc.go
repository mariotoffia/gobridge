// Package filter implements the predicate-based processor that drops
// envelopes failing a configured set of conditions before they reach
// the rest of the route pipeline.
//
// Responsibility:
//   - evaluate a list of MatchCondition predicates against each
//     envelope and decide whether the envelope continues down the
//     processor chain or is dropped (with optional metric / log)
//   - reuse the runtime condition evaluator semantics so filter and
//     resolver behaviour stay symmetrical
//
// Key types:
//   - Processor: the ports.Processor implementation
//   - Config: the predicate set and drop policy
//
// Dependencies: ports (Processor), domain/messaging (Envelope), and
// the runtime condition evaluator types. No transport or storage
// dependencies.
package filter
