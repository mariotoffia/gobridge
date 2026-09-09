package main

import (
	"context"
	"encoding/json"
	"github.com/mariotoffia/gobridge/ports"
)

type controlPlaneLog struct{ ready chan [2]string }

func (l *controlPlaneLog) Write(data []byte) (int, error) {
	var entry struct{ Msg, Admin, Monitor string }
	if json.Unmarshal(data, &entry) == nil && entry.Msg == "control plane started" {
		select {
		case l.ready <- [2]string{entry.Admin, entry.Monitor}:
		default:
		}
	}
	return len(data), nil
}

type observationSource struct{ changes chan ports.ConfigObservation }

func (s *observationSource) Load(context.Context) (*ports.BridgeConfig, error) {
	return nil, context.Canceled
}
func (s *observationSource) Observe(context.Context) (<-chan ports.ConfigObservation, error) {
	return s.changes, nil
}
