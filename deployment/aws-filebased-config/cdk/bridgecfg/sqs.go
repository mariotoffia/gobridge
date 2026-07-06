package bridgecfg

import (
	"fmt"

	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/ports"
)

// SQSReceiverOption mutates the *sqs.Config the builder synthesises
// before attaching it to the BridgeConfig. Options are applied in
// order so a later option can override an earlier one (e.g. defaults
// first, then operator overrides).
type SQSReceiverOption func(*sqs.Config)

// SQSSenderOption mutates the *sqs.Config the builder synthesises
// for an SQS sender. The same Config type is reused for both roles
// because the adapter shares it; the sender option type is kept
// distinct so the public API is self-documenting.
type SQSSenderOption func(*sqs.Config)

// WithSQSReceiver adds an SQS receiver under the given logical id.
//
// QueueURL is taken from ref.Queue().QueueUrl() when the registry
// resolved a real CDK handle; otherwise QueueName falls back to
// ref.Name() and Phase-2 validation surfaces the missing
// registration. The builder itself never errors on an unresolved
// ref — that decision lives in the construct so a single synth pass
// can collect every miss.
//
// AutoExtend is seeded with DefaultSQSAutoExtend() so the produced
// bridge.yaml is self-describing rather than leaning on the adapter's
// implicit fallback.
func (b *Builder) WithSQSReceiver(id string, ref registry.QueueRef, opts ...SQSReceiverOption) *Builder {
	if !b.reserveID(b.receiverIDs, "receiver", id) {
		return b
	}
	cfg := newSQSConfig(ref)
	cfg.AutoExtend = DefaultSQSAutoExtend()
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	if err := cfg.Validate(); err != nil {
		b.fail(fmt.Errorf("bridgecfg: receiver %q: %w", id, err))
		return b
	}
	// Synth-time completeness guard: a receiver must resolve a queue.
	// Parse-time Validate no longer requires one (binding overrides
	// share the Config shape), so enforce it here where the ref is
	// known to be a top-level receiver.
	if err := cfg.ValidateQueue(); err != nil {
		b.fail(fmt.Errorf("bridgecfg: receiver %q: %w", id, err))
		return b
	}
	def := ports.ReceiverDef{ID: id, Transport: sqsTransport}
	def.SetDecoded(cfg, nil)
	b.cfg.Receivers = append(b.cfg.Receivers, def)
	return b
}

// WithSQSSender adds an SQS sender under the given logical id. URL
// resolution semantics match WithSQSReceiver.
func (b *Builder) WithSQSSender(id string, ref registry.QueueRef, opts ...SQSSenderOption) *Builder {
	if !b.reserveID(b.senderIDs, "sender", id) {
		return b
	}
	cfg := newSQSConfig(ref)
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	if err := cfg.Validate(); err != nil {
		b.fail(fmt.Errorf("bridgecfg: sender %q: %w", id, err))
		return b
	}
	// Same synth-time queue guard as WithSQSReceiver.
	if err := cfg.ValidateQueue(); err != nil {
		b.fail(fmt.Errorf("bridgecfg: sender %q: %w", id, err))
		return b
	}
	def := ports.SenderDef{ID: id, Transport: sqsTransport}
	def.SetDecoded(cfg, nil)
	b.cfg.Senders = append(b.cfg.Senders, def)
	return b
}

// WithSQSRegion is the canonical option for steering the AWS region
// of an SQS receiver/sender. Provided as a convenience because every
// non-default deployment needs it.
func WithSQSRegion(region string) SQSReceiverOption {
	return func(c *sqs.Config) { c.Region = region }
}

// WithSQSSenderRegion is the sender-typed alias for WithSQSRegion.
// Functional options carry their role in the type so the compiler
// catches accidental misuse on the wrong builder method.
func WithSQSSenderRegion(region string) SQSSenderOption {
	return func(c *sqs.Config) { c.Region = region }
}

// sqsTransport is the discriminator the adapter registers under the
// short form. Kept as a package constant so the builder, the secret
// scanner, and round-trip tests share a single source of truth.
const sqsTransport = "sqs"

// newSQSConfig builds a *sqs.Config seeded from a queue ref, choosing
// QueueURL when the ref is resolved (CDK handle available at synth
// time) and falling back to QueueName otherwise. ref.Name() may be
// empty for the zero-value ref; that surfaces as a sqs.Validate error
// when the option chain finishes, which the builder converts into a
// receiver/sender-scoped Build error.
//
// The seed is sqs.DefaultConfig() (max_messages=10, wait_time_seconds=20),
// not a bare Config{}: the canonical config the builder marshals is the
// authoritative deployment artifact, and the plugin decode surface rejects
// an EXPLICIT wait_time_seconds:0 / max_messages:0 (short-polling is
// unsupported — see sqs/register.go). Emitting the documented defaults
// keeps the produced YAML valid on round-trip.
func newSQSConfig(ref registry.QueueRef) *sqs.Config {
	cfg := sqs.DefaultConfig()
	if ref.IsResolved() {
		if u := ref.Queue().QueueUrl(); u != nil {
			cfg.QueueURL = *u
			return &cfg
		}
	}
	cfg.QueueName = ref.Name()
	return &cfg
}
