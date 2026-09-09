package main

import (
	"context"

	"github.com/mariotoffia/gobridge/ports"
)

// Optional local seams for deterministic lifecycle fault injection. The command
// uses its ordinary initializer and Supervisor when no override is supplied.
type observedConfigHooks struct {
	initialize func(context.Context) error
	start      func(*ports.BridgeConfig, uint64) (*configSession, error)
}
