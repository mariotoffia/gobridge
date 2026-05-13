package config

import "github.com/mariotoffia/gobridge/ports"

// rawMapView is satisfied by ports.RawConfig implementations that can
// expose their underlying option map for read-only diagnostic peeks
// (e.g. validateStaleClaimDuration). The parser package's concrete
// rawMapConfig satisfies this through its AsMap method; alternative
// RawConfig implementations may return nil and the peek is skipped.
//
// Defining the interface in the model side keeps the dependency graph
// inward: the model never imports the parser; instead the parser's
// concrete type opts into the contract structurally.
type rawMapView interface {
	AsMap() map[string]any
}

// rawMap returns the underlying map[string]any if r satisfies
// rawMapView, or nil otherwise. Used by validators that inspect a
// single option without committing to the strict, all-or-nothing
// typed Decode contract.
func rawMap(r ports.RawConfig) map[string]any {
	v, ok := r.(rawMapView)
	if !ok || v == nil {
		return nil
	}
	return v.AsMap()
}
