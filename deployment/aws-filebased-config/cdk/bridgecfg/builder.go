package bridgecfg

import (
	"errors"
	"fmt"

	"github.com/mariotoffia/gobridge/ports"
)

// Builder is the fluent, type-safe assembler for a *ports.BridgeConfig.
//
// All With* methods return the receiver so calls can be chained. The
// builder accumulates errors internally and surfaces them once from
// Build; intermediate methods never panic on bad input. The zero
// Builder is not usable — always construct via New.
//
// A Builder is single-shot: callers compose a config in one chain and
// call Build exactly once. Re-using a Builder after Build is allowed
// (Build is idempotent and does not mutate the working set) but not
// recommended; mutating a returned *ports.BridgeConfig and calling
// Build again will see the mutation.
//
// Builder is NOT safe for concurrent use.
type Builder struct {
	cfg ports.BridgeConfig

	// err is the first error captured by any With* method. Build
	// returns it verbatim so the caller sees the originating
	// problem, not a downstream symptom.
	err error

	// id sets keep duplicate-ID detection cheap and deterministic
	// — the synth pass is single-threaded and short, so a small
	// map per kind is the right shape.
	sessionIDs  map[string]struct{}
	receiverIDs map[string]struct{}
	senderIDs   map[string]struct{}
	bindingIDs  map[string]struct{}
	routeIDs    map[string]struct{}

	// scanSecrets toggles the plaintext-secrets pass run from
	// Build. On by default; tests that want to assert raw builder
	// output without the scanner intercepting can disable it via
	// disableSecretScan (test-only helper).
	scanSecrets bool
}

// New returns a Builder seeded with the given bridge ID. The ID
// becomes BridgeConfig.Bridge.ID and is required — an empty name is
// captured as a Build-time error rather than panicking, so the fluent
// chain remains uninterrupted.
func New(name string) *Builder {
	b := &Builder{
		sessionIDs:  map[string]struct{}{},
		receiverIDs: map[string]struct{}{},
		senderIDs:   map[string]struct{}{},
		bindingIDs:  map[string]struct{}{},
		routeIDs:    map[string]struct{}{},
		scanSecrets: true,
	}
	if name == "" {
		b.fail(errors.New("bridgecfg: New: bridge name must not be empty"))
		return b
	}
	b.cfg.Bridge.ID = name
	return b
}

// Build finalises the config and returns the assembled
// *ports.BridgeConfig. Any error captured during the chain is
// returned verbatim; in addition the plaintext-secret scanner is run
// against the final config so inline credentials surface as a Build
// error rather than escaping into the synthesized bridge.yaml.
//
// Build does not call config.Validate — heavyweight cross-reference
// checks are the construct's responsibility (Phase 2 validator) so
// the builder remains usable in unit tests that exercise partial
// configs.
func (b *Builder) Build() (*ports.BridgeConfig, error) {
	if b.err != nil {
		return nil, b.err
	}
	if b.scanSecrets {
		if err := ScanForPlaintextSecrets(&b.cfg); err != nil {
			return nil, fmt.Errorf("bridgecfg: build: %w", err)
		}
	}
	cfg := b.cfg
	return &cfg, nil
}

// fail records err as the builder's terminal error if no earlier
// error was captured. Subsequent failures are dropped on purpose:
// the originating problem is the most actionable signal, and a chain
// after a broken step often produces noise rather than insight.
func (b *Builder) fail(err error) {
	if b.err == nil {
		b.err = err
	}
}

// reserveID checks set for name, registers it on success, and records
// a duplicate-ID error otherwise. kind is the human-readable bucket
// label included in the error message.
func (b *Builder) reserveID(set map[string]struct{}, kind, name string) bool {
	if name == "" {
		b.fail(fmt.Errorf("bridgecfg: %s id must not be empty", kind))
		return false
	}
	if _, ok := set[name]; ok {
		b.fail(fmt.Errorf("bridgecfg: duplicate %s id %q", kind, name))
		return false
	}
	set[name] = struct{}{}
	return true
}

// disableSecretScan turns off the plaintext-secret pass that Build
// otherwise runs. Test-only helper kept lower-case so it does not
// leak into the public surface; secrets_test.go and builder_test.go
// touch it through reflection-free package-internal access.
func (b *Builder) disableSecretScan() *Builder {
	b.scanSecrets = false
	return b
}
