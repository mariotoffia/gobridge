// Package config implements the YAML/JSON parser, validator, merger,
// and on-disk manager that produce *ports.BridgeConfig values from
// configuration sources.
//
// The parsed type definitions themselves live in `ports/` (see
// ports.BridgeConfig and the *Def sub-types) so the bridge runtime
// and the admin HTTP layer can consume them without taking a
// dependency on this parser package or on yaml.v3.
package config
