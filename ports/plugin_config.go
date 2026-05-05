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
	"sync"
)

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
// not usable; construct via NewRegistry or use DefaultRegistry.
type Registry struct {
	mu       sync.RWMutex
	decoders map[string]ConfigDecoder
}

// NewRegistry returns an empty, ready-to-use Registry.
func NewRegistry() *Registry {
	return &Registry{decoders: map[string]ConfigDecoder{}}
}

// Register associates a decoder with a kind. It panics if kind is
// already registered: duplicate registration is always a programming
// error (typically a copy/paste init() in two adapters claiming the
// same discriminator) and must be caught at process start.
func (r *Registry) Register(kind string, dec ConfigDecoder) {
	if dec == nil {
		panic("ports: nil ConfigDecoder for kind " + kind)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.decoders == nil {
		r.decoders = map[string]ConfigDecoder{}
	}
	if _, dup := r.decoders[kind]; dup {
		panic("ports: duplicate plugin kind " + kind)
	}
	r.decoders[kind] = dec
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

// DefaultRegistry is the process-wide registry adapters self-register
// into from their init() functions. The composition root imports the
// adapters it wants (`_ "…/adapters/aws/transport/sqs"`); the
// import graph determines which kinds are available at runtime.
var DefaultRegistry = NewRegistry()

// Factory is the runtime-side counterpart of PluginConfig: a typed
// constructor an adapter exposes for its own concrete config. The
// runtime calls `factory(ctx, cfg)` exactly once at composition,
// receiving the adapter's concrete type without reflection or map
// lookups.
type Factory[C PluginConfig, P any] func(ctx context.Context, cfg C) (P, error)
