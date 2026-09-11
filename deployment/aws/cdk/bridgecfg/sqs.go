package bridgecfg

import (
	"fmt"

	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/deployment/aws/cdk/registry"
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
// Explicitly bound QueueTags take precedence; otherwise the known physical
// QueueName is used. Generated names require BindQueueTags or an explicit
// queue_url option for non-embedded consumers. An unresolved ref retains its
// name so the construct's annotation pass can report missing registrations.
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
		b.fail(fmt.Errorf("bridgecfg: receiver %q: %w; use a stable physical QueueName or registry.BindQueueTags", id, err))
		return b
	}
	def := ports.ReceiverDef{ID: id, Transport: sqsTransport}
	def.SetDecoded(cfg.FreezePluginConfig(), nil)
	b.cfg.Receivers = append(b.cfg.Receivers, def)
	return b
}

// WithSQSSender adds an SQS sender under the given logical id. Queue
// selection semantics match WithSQSReceiver.
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
		b.fail(fmt.Errorf("bridgecfg: sender %q: %w; use a stable physical QueueName or registry.BindQueueTags", id, err))
		return b
	}
	def := ports.SenderDef{ID: id, Transport: sqsTransport}
	def.SetDecoded(cfg.FreezePluginConfig(), nil)
	b.cfg.Senders = append(b.cfg.Senders, def)
	// Keep the binding address consistent with the logical selector; a
	// tag-selected queue has no physical name to serialize during synth.
	b.senderAddresses[id] = sqsSenderAddress(cfg, ref)
	return b
}

// sqsSenderAddress is the queue an SQS sender sends to, in the form a binding
// address may carry it.
//
// Tag-selected senders use the adapter's configured-queue address. Direct
// URLs take precedence over names, matching runtime resolution. Registry
// aliases are used only for unresolved refs, which Phase 2 will reject.
func sqsSenderAddress(cfg *sqs.Config, ref registry.QueueRef) string {
	if cfg.QueueTags != nil {
		return sqs.QueueAddress
	}
	if cfg.QueueURL != "" {
		return cfg.QueueURL
	}
	if cfg.QueueName != "" {
		return cfg.QueueName
	}
	return ref.Name()
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
const sqsTransport = sqs.ShortKind

// newSQSConfig never turns a registry alias into a physical queue name or
// embeds an unresolved QueueUrl token. Unknown generated names need an
// explicit selector binding. Options can still set QueueURL for consumers
// that intentionally use a deploy-time-resolved config.
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
		cfg.QueueTags = ref.QueueTags()
		if cfg.QueueTags != nil {
			cfg.QueueNamePrefix = ref.QueueNamePrefix()
		} else {
			cfg.QueueName = ref.PhysicalName()
		}
		return &cfg
	}
	cfg.QueueName = ref.Name()
	return &cfg
}
