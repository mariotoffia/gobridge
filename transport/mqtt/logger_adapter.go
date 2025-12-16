package mqtt

import (
	"context"
	"fmt"

	"github.com/eclipse/paho.golang/paho/log"
	"github.com/mariotoffia/gobridge/bridge/types"
)

// PahoLoggerAdapter adapts LogCreator to paho's log.Logger interface.
// All paho logs are emitted at Debug level since they are typically
// verbose internal protocol messages.
type PahoLoggerAdapter struct {
	Log types.LogCreator
	ctx context.Context
}

// Ensure PahoLoggerAdapter implements paho's log.Logger interface
var _ log.Logger = (*PahoLoggerAdapter)(nil)

// NewPahoLoggerAdapter creates a new adapter that bridges paho logging to types.Logger.
func NewPahoLoggerAdapter(log types.LogCreator, ctx context.Context) *PahoLoggerAdapter {
	return &PahoLoggerAdapter{Log: log, ctx: ctx}
}

// Println implements log.Logger interface.
func (a *PahoLoggerAdapter) Println(v ...interface{}) {
	if a.Log != nil {
		a.Log(a.ctx, types.LogLevelDebug).Str("source", "paho").Msg(fmt.Sprint(v...))
	}
}

// Printf implements log.Logger interface.
func (a *PahoLoggerAdapter) Printf(format string, v ...interface{}) {
	if a.Log != nil {
		a.Log(a.ctx, types.LogLevelDebug).Str("source", "paho").Msgf(format, v...)
	}
}
