// Package transform implements the configurable header / payload
// transformation processor used to remap incoming envelopes into the
// shape expected downstream.
//
// Responsibility:
//   - apply a declarative mapping (header rewrites, payload field
//     copies / projections) to each envelope flowing through a route
//   - keep transformations side-effect free with respect to the
//     original envelope: mutated envelopes are produced via the
//     messaging package's controlled mutation API rather than direct
//     map writes
//
// Key types:
//   - Processor: the ports.Processor implementation
//   - Config / Mapping: declarative description of header and payload
//     transformations
//
// Dependencies: ports (Processor) and domain/messaging (Envelope and
// Headers). No transport or storage dependencies.
package transform
