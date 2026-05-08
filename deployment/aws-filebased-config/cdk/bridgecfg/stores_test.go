package bridgecfg_test

import (
	"strings"
	"testing"

	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
)

func TestWithSQLiteOutbox_Populated(t *testing.T) {
	cfg, err := bridgecfg.New("b").
		WithSQLiteOutbox("/mnt/state/outbox.db").
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if cfg.Stores.Outbox == nil {
		t.Fatal("Stores.Outbox is nil")
	}
	if cfg.Stores.Outbox.Type != "sqlite" {
		t.Errorf("Outbox.Type = %q, want sqlite", cfg.Stores.Outbox.Type)
	}
	sc, ok := cfg.Stores.Outbox.Config.(*nativestore.SQLiteConfig)
	if !ok {
		t.Fatalf("Outbox.Config = %T, want *nativestore.SQLiteConfig", cfg.Stores.Outbox.Config)
	}
	if sc.Path != "/mnt/state/outbox.db" {
		t.Errorf("Path = %q, want /mnt/state/outbox.db", sc.Path)
	}
}

func TestWithSQLiteLeaseAndDLQ(t *testing.T) {
	cfg, err := bridgecfg.New("b").
		WithSQLiteLease("/mnt/state/lease.db").
		WithSQLiteDLQ("/mnt/state/dlq.db").
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if cfg.Stores.Lease == nil || cfg.Stores.DLQ == nil {
		t.Fatalf("expected both stores populated; got Lease=%v DLQ=%v",
			cfg.Stores.Lease, cfg.Stores.DLQ)
	}
	if cfg.Stores.Lease.Config.(*nativestore.SQLiteConfig).Path != "/mnt/state/lease.db" {
		t.Errorf("Lease path mismatch")
	}
	if cfg.Stores.DLQ.Config.(*nativestore.SQLiteConfig).Path != "/mnt/state/dlq.db" {
		t.Errorf("DLQ path mismatch")
	}
}

func TestWithSQLiteOutbox_EmptyPath_BuildErrors(t *testing.T) {
	_, err := bridgecfg.New("b").
		WithSQLiteOutbox("").
		Build()
	if err == nil {
		t.Fatal("expected error on empty path")
	}
	if !strings.Contains(err.Error(), "sqlite") || !strings.Contains(err.Error(), "outbox") {
		t.Errorf("error = %v, want one mentioning sqlite outbox", err)
	}
}
