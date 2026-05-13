package config

import "github.com/mariotoffia/gobridge/ports"

// fakeRawConfig is a test-only ports.RawConfig used by model tests
// (merge, validate_s12) that need to attach a raw options map to a
// blueprint def without pulling in config/parser. It satisfies the
// rawMapView contract so validateStaleClaimDuration and merge
// preservation assertions exercise the same diagnostic path the
// production parser would.
//
// Decode is intentionally a no-op: model tests assert only on the
// AsMap-side diagnostics, never on typed plugin decoding (which lives
// in the parser package's tests).
type fakeRawConfig map[string]any

var _ ports.RawConfig = fakeRawConfig(nil)

func (f fakeRawConfig) Decode(_ any) error    { return nil }
func (f fakeRawConfig) AsMap() map[string]any { return f }
