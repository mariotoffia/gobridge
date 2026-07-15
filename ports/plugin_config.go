// Package ports — plugin config contract.
//
// PluginConfig is the marker interface every pluggable component
// (transport, store, processor, credential source, etc.) implements
// with its own concrete config struct. The two-stage parser in
// `config/` resolves a discriminator (e.g. `transport: aws.sqs`),
// looks up the registered ConfigDecoder in the Registry, and asks it
// to turn an opaque RawConfig into the typed PluginConfig.
//
// RawConfig is interface-only inside `ports/`; the concrete
// implementation that wraps yaml/json stage-1 output lives in
// `config/` so encoding libraries stay out of the inner ring.
package ports

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// ErrDuplicateKind is returned by Registry.Register when the supplied
// kind has already been registered. It is a typed *shared.BridgeError
// (Code: ErrCodeAlreadyExists, Class: ErrorPermanent) so callers can
// errors.Is for it and so the same diagnostics infrastructure that
// classifies runtime errors classifies registration errors.
var ErrDuplicateKind = shared.NewBridgeError(
	shared.ErrCodeAlreadyExists,
	shared.ErrorPermanent,
	"ports: duplicate plugin kind",
)

// ErrNilDecoder is returned by Registry.Register when the supplied
// ConfigDecoder is nil. Same typed-error rationale as ErrDuplicateKind.
// It carries Code ErrCodeInvalidConfig (a registration/config defect a
// human must fix), NOT ErrCodeInvalidPayload — registering a nil decoder
// is a configuration error, not a rejected message payload. This keeps
// ErrCodeInvalidPayload uniquely ErrorRejected across the contract layer.
var ErrNilDecoder = shared.NewBridgeError(
	shared.ErrCodeInvalidConfig,
	shared.ErrorPermanent,
	"ports: nil ConfigDecoder",
)

// DurableSessionIdentityConfig is an OPTIONAL interface for typed transport
// configs whose broker-side session state can outlive a process. The returned
// value is an opaque, secret-safe fingerprint of every field that selects that
// durable state. It must remain stable for the same effective config within a
// process. Callers compare it for equality only and must not log identity input.
type DurableSessionIdentityConfig interface {
	// DurableSessionIdentity fingerprints all effective broker-state fields and
	// is used to reject state-stranding reload changes.
	DurableSessionIdentity(mode connectivity.SessionMode) (string, error)
	// DurableSessionIdentityDomains returns one opaque ownership key per
	// canonical broker endpoint combined with the effective client identity.
	// Any shared key means two sessions can connect as the same broker client.
	DurableSessionIdentityDomains(mode connectivity.SessionMode) ([]string, error)
}

// SessionActivationTiming reports one conservative effective bound for the
// complete sequential activation path after an exclusive lease is acquired but
// before the session can converge. The composition root installs that bound on
// the runtime manager, which renews the lease throughout activation.
type SessionActivationTiming struct {
	// WorstCaseDuration is the conservative hard bound for every sequential
	// phase that may run between exclusive lease acquisition and convergence.
	// Nested per-attempt limits must not be double-counted.
	WorstCaseDuration time.Duration
}

// PostAcquireActivationTimingConfig is an OPTIONAL typed-config capability for
// stateful transports whose initial/replacement reconciliation can include a
// mandatory replay-verification wait. The returned duration must be conservative,
// effective (defaults already resolved), and non-secret. It is zero when the
// selected session mode has no post-acquire durable activation sequence.
type PostAcquireActivationTimingConfig interface {
	PostAcquireActivationTiming(mode connectivity.SessionMode) SessionActivationTiming
}

// TransportFailoverTiming reports conservative effective durations used by a
// replacement owner between lease acquisition and ServiceLevelFull readiness.
type TransportFailoverTiming struct {
	BrokerConnectTimeout time.Duration
	ReconcileTimeout     time.Duration
}

// TransportFailoverTimingConfig is an OPTIONAL generic typed-config capability.
// A declared failover SLO requires both durations to be known and positive.
type TransportFailoverTimingConfig interface {
	TransportFailoverTiming(mode connectivity.SessionMode) TransportFailoverTiming
}

// FreezableConfig is an OPTIONAL adapter-owned capability for typed plugin
// configs that need an isolated snapshot across asynchronous validation/build
// boundaries. FreezePluginConfig must return a deep-owned copy of every mutable
// configuration value while intentionally sharing only opaque immutable or
// runtime dependencies that must retain identity (for example a client handle,
// clock, mutex-bearing state, or process-stable suffix resolver). The result must
// be non-nil, retain the source Kind and this freeze capability, and preserve
// any durable-identity, post-acquire timing, failover timing, and credential capabilities
// exposed by the source.
type FreezableConfig interface {
	FreezePluginConfig() PluginConfig
}

// IsNilPluginConfig detects a nil interface and an interface holding a typed nil
// pointer/map/slice/function/channel. Capability callers must check this before
// invoking methods whose value-receiver method set can otherwise panic.
func IsNilPluginConfig(config PluginConfig) bool {
	if config == nil {
		return true
	}
	value := reflect.ValueOf(config)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// Replica identity strategy values understood by transport-neutral validation.
const (
	// ReplicaIdentityHostname is a stable per-replica identity derived from the
	// process hostname (for example, a Kubernetes pod name).
	ReplicaIdentityHostname = "hostname"
	// ReplicaIdentityNonce is a process-unique identity suitable only when
	// broker-side session state is intentionally ephemeral.
	ReplicaIdentityNonce = "nonce"
)

// ReplicaIdentityConfig is an OPTIONAL interface for typed transport configs
// that support per-replica connection identities. The strategy is declarative:
// validation uses it to prove clustered shared consumers cannot collide before
// any transport is built. Empty means no per-replica identity strategy.
type ReplicaIdentityConfig interface {
	ReplicaIdentityStrategy() string
}

// PluginConfig is the marker interface every plugin implements with
// its own concrete config struct. Implementations are typically a
// plain Go struct with yaml/json tags and a Validate() method that
// runs once during config parsing.
type PluginConfig interface {
	// Kind returns the discriminator used in the blueprint and the
	// registry key under which the decoder is registered. It must
	// match the value the user writes under `transport:` / `type:` in
	// the blueprint.
	Kind() string

	// Validate checks the config for completeness and consistency.
	// Validate must be a pure function: no I/O, no network, no
	// goroutines. It runs once during config parsing.
	Validate() error
}

// PublishingConfig is an OPTIONAL interface a transport's typed sender
// PluginConfig may implement to advertise the topic/exchange it publishes to,
// mirroring the CredentialedConfig optional-config idiom. The
// transport-neutral bridge cannot read a transport-specific field (e.g.
// amqp091.Config.Sender.Exchange) without importing the adapter (forbidden by
// .go-arch-lint.yml), so it type-asserts to this accessor to thread the name
// into connectivity.PublisherPlan.Topic — which the adapter then declares on a
// best-effort basis (a declare that the broker rejects, e.g. against an
// externally-managed or least-privilege exchange, must not tear the session
// down; publishing still works when the exchange already exists). Transports
// whose broker needs no publish-side topology (MQTT, SQS, Service Bus) simply
// don't implement it.
type PublishingConfig interface {
	PublisherTopic() string

	// PublisherTopologyKey returns a deterministic descriptor of the exchange
	// DECLARATION topology this config contributes (exchange type, durability,
	// auto-delete, and the exchange-argument table — NOT the routing key, which
	// is a per-message property and legitimately differs between senders). The
	// bridge dedups senders by PublisherTopic() keeping the first (broker
	// first-declare-wins); it compares this key to tell a legitimate identical
	// re-declaration of an already-advertised exchange (stays silent) from a
	// genuinely DIVERGENT one (logged as a misconfiguration — REV-2-topowarn).
	// The comparison is transport-neutral: the bridge never reads adapter fields
	// directly. A config that declares no publish-side topology returns "".
	PublisherTopologyKey() string
}

// RawConfig is an opaque carrier for not-yet-decoded plugin options.
// It is produced by the config-source adapter (yaml/json/etc.) and
// consumed by exactly one plugin decoder. It MUST NOT cross any
// other boundary.
type RawConfig interface {
	// Decode populates target from the raw payload. Implementations
	// typically delegate to yaml.Unmarshal or an equivalent
	// structural decoder.
	Decode(target any) error
}

// ConfigDecoder turns a RawConfig into a typed PluginConfig. Decoders
// are registered per-kind on a Registry; the typical implementation
// decodes into the adapter's concrete Config struct, calls Validate,
// and returns the value.
type ConfigDecoder func(raw RawConfig) (PluginConfig, error)

// Registry maps plugin kinds to their decoders. The zero value is
// not usable; construct via NewRegistry. Each composition root owns
// its own Registry — there is no process-wide singleton. Adapters
// expose an exported Register(reg *ports.Registry) error function
// from their register.go which the composition root calls explicitly
// after constructing the registry.
type Registry struct {
	mu       sync.RWMutex
	decoders map[string]ConfigDecoder
}

// NewRegistry returns an empty, ready-to-use Registry.
func NewRegistry() *Registry {
	return &Registry{decoders: map[string]ConfigDecoder{}}
}

// Register associates a decoder with a kind. It returns ErrNilDecoder
// when dec is nil and ErrDuplicateKind when kind is already
// registered. The returned error is wrapped with the offending kind
// for diagnostics; callers can recover the sentinel via errors.Is.
//
// Duplicate registration is treated as a programming error (typically
// a copy/paste in two adapter Register functions claiming the same
// discriminator) and must be surfaced at composition time, but the
// registry no longer panics — callers decide whether to fail-fast or
// degrade.
func (r *Registry) Register(kind string, dec ConfigDecoder) error {
	if dec == nil {
		return fmt.Errorf("%w: kind %q", ErrNilDecoder, kind)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.decoders == nil {
		r.decoders = map[string]ConfigDecoder{}
	}
	if _, dup := r.decoders[kind]; dup {
		return fmt.Errorf("%w: kind %q", ErrDuplicateKind, kind)
	}
	r.decoders[kind] = dec
	return nil
}

// Decode looks up the decoder for kind and runs it against raw. It
// returns an "unknown plugin kind" error when no decoder is
// registered. Errors from the decoder are surfaced unchanged so
// callers can wrap them with their own attachment-point context.
func (r *Registry) Decode(kind string, raw RawConfig) (PluginConfig, error) {
	r.mu.RLock()
	dec, ok := r.decoders[kind]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("ports: unknown plugin kind %q", kind)
	}
	return dec(raw)
}

// Kinds returns the sorted list of every plugin kind currently
// registered. It is intended for tooling (CI checks, diagnostics,
// documentation generators) that needs to enumerate the registered
// discriminators after the desired adapter side-effect imports have
// run. It is not part of any hot path; the snapshot is taken under
// the registry's read lock and returned as a fresh slice the caller
// owns.
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	out := make([]string, 0, len(r.decoders))
	for k := range r.decoders {
		out = append(out, k)
	}
	r.mu.RUnlock()
	sort.Strings(out)
	return out
}

// Factory is the runtime-side counterpart of PluginConfig: a typed
// constructor an adapter exposes for its own concrete config. The
// runtime calls `factory(ctx, cfg)` exactly once at composition,
// receiving the adapter's concrete type without reflection or map
// lookups.
type Factory[C PluginConfig, P any] func(ctx context.Context, cfg C) (P, error)
