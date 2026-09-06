//go:build gobridge_native || gobridge_all

package main

import (
	"context"
	"log/slog"

	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/ports"
)

//nolint:gochecknoinits // Records build-tag metadata only; decoder and factory registration remain explicit.
func init() { compiledFamilies = append(compiledFamilies, "native") }

func registerNativeDecoders(reg *ports.Registry) error {
	return nativestore.Register(reg)
}

func wireNativeFactories(_ context.Context, sup *bridge.Supervisor, _ *slog.Logger, _ ports.MetricsExporter) error {
	sup.RegisterStoreFactory("memory", nativestore.NewMemoryStoreFactory())
	sup.RegisterStoreFactory("sqlite", nativestore.NewSQLiteStoreFactory())
	return nil
}

func seedNativeStores(_ context.Context, b *bridge.Builder) error {
	b.RegisterStoreFactory("memory", nativestore.NewMemoryStoreFactory())
	b.RegisterStoreFactory("sqlite", nativestore.NewSQLiteStoreFactory())
	return nil
}
