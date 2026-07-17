package dynamodblease

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
)

func TestAcquire_PreObservationLegacyRowRequiresCompleteBaseTuple(t *testing.T) {
	const ttl = 20 * time.Second
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	row := validExistingLeaseRow(base, ttl)
	delete(row, attrRenewedAt)
	client := &observationMemoryClient{item: row}
	store := &Store{client: client, tableName: "leases", clk: clocktest.NewAt(base)}
	if _, err := store.Acquire(t.Context(), "lease-1", "standby", ttl, nil); !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("missing base tuple field: %v", err)
	}
}

func TestAcquire_ModernTakeoverFencesExactTupleAndObservationEvidence(t *testing.T) {
	const ttl = 20 * time.Second
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client := &seizeClient{item: heldRow("owner", 7, base, ttl)}
	clk := clocktest.NewAt(base)
	store := &Store{client: client, tableName: "leases", clk: clk}
	if _, err := store.Acquire(t.Context(), "l1", "standby", ttl, nil); !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("first observation: %v", err)
	}
	clk.Advance(ttl)
	token, err := store.Acquire(t.Context(), "l1", "standby", ttl, nil)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if token.Version != 8 {
		t.Fatalf("version=%d want 8", token.Version)
	}
	condition := client.lastUpdateExp
	for _, required := range []string{"#pk = :tuple_pk", "#own = :tuple_owner", "#ver = :tuple_version", "#ren = :tuple_renewed", "#exp = :tuple_expires", "#obs_fp = :obs_fp", "#obs_elapsed = :obs_elapsed", "#obs_gen = :obs_gen"} {
		if !strings.Contains(condition, required) {
			t.Fatalf("takeover condition %q missing %q", condition, required)
		}
	}
}
