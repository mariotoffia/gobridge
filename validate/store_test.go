package validate

import (
	"strings"
	"testing"
)

func TestValidateStoreBackends_ClusteredWithLocalLease(t *testing.T) {
	cfg := BridgeConfig{
		DeploymentMode:        "clustered",
		HasLeaseStore:         true,
		LeaseStoreDistributed: false,
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for local lease store in clustered mode")
	}
	if !strings.Contains(err.Error(), "distributed LeaseStore") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateStoreBackends_ClusteredWithLocalOutbox(t *testing.T) {
	cfg := BridgeConfig{
		DeploymentMode:         "clustered",
		HasOutboxStore:         true,
		OutboxStoreDistributed: false,
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for local outbox store in clustered mode")
	}
	if !strings.Contains(err.Error(), "distributed OutboxStore") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateStoreBackends_ClusteredWithLocalDLQ(t *testing.T) {
	cfg := BridgeConfig{
		DeploymentMode:      "clustered",
		HasDLQStore:         true,
		DLQStoreDistributed: false,
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for local DLQ store in clustered mode")
	}
	if !strings.Contains(err.Error(), "distributed DLQStore") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateStoreBackends_NonClustered_NoErrors(t *testing.T) {
	cfg := BridgeConfig{
		DeploymentMode: "standalone",
		HasLeaseStore:  true,
		HasOutboxStore: true,
		HasDLQStore:    true,
	}
	err := Validate(cfg)
	if err != nil {
		t.Fatalf("expected no errors in standalone mode, got: %v", err)
	}
}

func TestValidateStoreBackends_ClusteredAllDistributed_NoErrors(t *testing.T) {
	cfg := BridgeConfig{
		DeploymentMode:         "clustered",
		HasLeaseStore:          true,
		LeaseStoreDistributed:  true,
		HasOutboxStore:         true,
		OutboxStoreDistributed: true,
		HasDLQStore:            true,
		DLQStoreDistributed:    true,
	}
	err := Validate(cfg)
	if err != nil {
		t.Fatalf("expected no errors with distributed stores, got: %v", err)
	}
}
