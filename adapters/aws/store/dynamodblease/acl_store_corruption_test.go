package dynamodblease

import (
	"errors"
	"strconv"
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/shared"
)

func validExistingLeaseRow(base time.Time, ttl time.Duration) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		attrPK:        &ddbtypes.AttributeValueMemberS{Value: leaseKey("lease-1")},
		attrOwner:     &ddbtypes.AttributeValueMemberS{Value: "owner-a"},
		attrVersion:   &ddbtypes.AttributeValueMemberN{Value: "7"},
		attrRenewedAt: &ddbtypes.AttributeValueMemberN{Value: millisStr(base)},
		attrExpiresAt: &ddbtypes.AttributeValueMemberN{Value: millisStr(base.Add(ttl))},
	}
}

func TestAcquire_CorruptExistingLeaseRowsFailClosed(t *testing.T) {
	const ttl = 20 * time.Second
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := map[string]func(map[string]ddbtypes.AttributeValue){
		"missing-pk": func(row map[string]ddbtypes.AttributeValue) { delete(row, attrPK) },
		"wrong-pk": func(row map[string]ddbtypes.AttributeValue) {
			row[attrPK] = &ddbtypes.AttributeValueMemberS{Value: leaseKey("other")}
		},
		"missing-owner":      func(row map[string]ddbtypes.AttributeValue) { delete(row, attrOwner) },
		"empty-active-owner": func(row map[string]ddbtypes.AttributeValue) { row[attrOwner] = &ddbtypes.AttributeValueMemberS{} },
		"missing-version":    func(row map[string]ddbtypes.AttributeValue) { delete(row, attrVersion) },
		"non-number-version": func(row map[string]ddbtypes.AttributeValue) {
			row[attrVersion] = &ddbtypes.AttributeValueMemberS{Value: "7"}
		},
		"zero-version": func(row map[string]ddbtypes.AttributeValue) {
			row[attrVersion] = &ddbtypes.AttributeValueMemberN{Value: "0"}
		},
		"max-version-cannot-increment": func(row map[string]ddbtypes.AttributeValue) {
			row[attrVersion] = &ddbtypes.AttributeValueMemberN{Value: "18446744073709551615"}
		},
		"overflow-version": func(row map[string]ddbtypes.AttributeValue) {
			row[attrVersion] = &ddbtypes.AttributeValueMemberN{Value: "18446744073709551616"}
		},
		"missing-renewed": func(row map[string]ddbtypes.AttributeValue) { delete(row, attrRenewedAt) },
		"non-number-renewed": func(row map[string]ddbtypes.AttributeValue) {
			row[attrRenewedAt] = &ddbtypes.AttributeValueMemberS{Value: "1"}
		},
		"zero-renewed": func(row map[string]ddbtypes.AttributeValue) {
			row[attrRenewedAt] = &ddbtypes.AttributeValueMemberN{Value: "0"}
		},
		"negative-renewed": func(row map[string]ddbtypes.AttributeValue) {
			row[attrRenewedAt] = &ddbtypes.AttributeValueMemberN{Value: "-1"}
		},
		"overflow-renewed": func(row map[string]ddbtypes.AttributeValue) {
			row[attrRenewedAt] = &ddbtypes.AttributeValueMemberN{Value: "9223372036854775808"}
		},
		"missing-expires": func(row map[string]ddbtypes.AttributeValue) { delete(row, attrExpiresAt) },
		"non-number-expires": func(row map[string]ddbtypes.AttributeValue) {
			row[attrExpiresAt] = &ddbtypes.AttributeValueMemberS{Value: "1"}
		},
		"negative-expires": func(row map[string]ddbtypes.AttributeValue) {
			row[attrExpiresAt] = &ddbtypes.AttributeValueMemberN{Value: "-1"}
		},
		"overflow-expires": func(row map[string]ddbtypes.AttributeValue) {
			row[attrExpiresAt] = &ddbtypes.AttributeValueMemberN{Value: "9223372036854775808"}
		},
		"overflow-liveness-duration": func(row map[string]ddbtypes.AttributeValue) {
			row[attrRenewedAt] = &ddbtypes.AttributeValueMemberN{Value: "1"}
			row[attrExpiresAt] = &ddbtypes.AttributeValueMemberN{Value: "9223372036854775807"}
		},
		"zero-active-expires": func(row map[string]ddbtypes.AttributeValue) {
			row[attrExpiresAt] = &ddbtypes.AttributeValueMemberN{Value: "0"}
		},
		"expiry-equals-renewed": func(row map[string]ddbtypes.AttributeValue) { row[attrExpiresAt] = row[attrRenewedAt] },
		"expiry-before-renewed": func(row map[string]ddbtypes.AttributeValue) {
			row[attrExpiresAt] = &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(base.Add(-time.Second).UnixMilli(), 10)}
		},
		"partial-observation-fingerprint": func(row map[string]ddbtypes.AttributeValue) {
			row[attrObservationFingerprint] = &ddbtypes.AttributeValueMemberS{Value: "fp"}
		},
		"partial-observation-duration": func(row map[string]ddbtypes.AttributeValue) {
			row[attrObservationElapsed] = &ddbtypes.AttributeValueMemberN{Value: "1"}
		},
		"partial-observation-generation": func(row map[string]ddbtypes.AttributeValue) {
			row[attrObservationGeneration] = &ddbtypes.AttributeValueMemberN{Value: "1"}
		},
		"non-string-observation-fingerprint": func(row map[string]ddbtypes.AttributeValue) {
			addObservationTestEvidence(row, "1", "1")
			row[attrObservationFingerprint] = &ddbtypes.AttributeValueMemberN{Value: "1"}
		},
		"non-number-observation-duration": func(row map[string]ddbtypes.AttributeValue) {
			addObservationTestEvidence(row, "1", "1")
			row[attrObservationElapsed] = &ddbtypes.AttributeValueMemberS{Value: "1"}
		},
		"zero-observation-generation":   func(row map[string]ddbtypes.AttributeValue) { addObservationTestEvidence(row, "1", "0") },
		"negative-observation-duration": func(row map[string]ddbtypes.AttributeValue) { addObservationTestEvidence(row, "-1", "1") },
		"overflow-observation-duration": func(row map[string]ddbtypes.AttributeValue) {
			addObservationTestEvidence(row, "9223372036854775808", "1")
		},
		"overflow-observation-generation": func(row map[string]ddbtypes.AttributeValue) {
			addObservationTestEvidence(row, "1", "18446744073709551616")
		},
		"max-observation-generation-cannot-increment": func(row map[string]ddbtypes.AttributeValue) {
			tuple := leaseTuple{owner: "owner-a", ownerPresent: true, version: 7, versionPresent: true,
				renewedAt: base.UnixMilli(), renewedAtPresent: true, expiresAt: base.Add(ttl).UnixMilli(), expiresAtPresent: true}
			row[attrObservationFingerprint] = &ddbtypes.AttributeValueMemberS{Value: tuple.fingerprint()}
			row[attrObservationElapsed] = &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(int64(ttl), 10)}
			row[attrObservationGeneration] = &ddbtypes.AttributeValueMemberN{Value: "18446744073709551615"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			row := validExistingLeaseRow(base, ttl)
			mutate(row)
			client := &observationMemoryClient{item: row}
			store := &Store{client: client, tableName: "leases", clk: clocktest.NewAt(base)}
			token, err := store.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
			if !errors.Is(err, shared.ErrInvalidConfig) {
				t.Fatalf("error=%v token=%+v, want typed corrupt-store ErrInvalidConfig", err, token)
			}
			client.mu.Lock()
			defer client.mu.Unlock()
			if owner := strAttr(client.item, attrOwner); owner == "standby" {
				t.Fatal("corrupt row was acquired")
			}
			if version, parseErr := optionalNumAttr(client.item, attrVersion); parseErr == nil && version == 1 {
				t.Fatal("corrupt row reset fencing version to 1")
			}
		})
	}
}

func addObservationTestEvidence(row map[string]ddbtypes.AttributeValue, elapsed, generation string) {
	row[attrObservationFingerprint] = &ddbtypes.AttributeValueMemberS{Value: "fp"}
	row[attrObservationElapsed] = &ddbtypes.AttributeValueMemberN{Value: elapsed}
	row[attrObservationGeneration] = &ddbtypes.AttributeValueMemberN{Value: generation}
}

func TestAcquire_WellFormedReleasedRowPreservesMonotonicVersion(t *testing.T) {
	const ttl = 20 * time.Second
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	row := validExistingLeaseRow(base, ttl)
	row[attrOwner] = &ddbtypes.AttributeValueMemberS{Value: ""}
	row[attrExpiresAt] = &ddbtypes.AttributeValueMemberN{Value: "0"}
	client := &observationMemoryClient{item: row}
	store := &Store{client: client, tableName: "leases", clk: clocktest.NewAt(base.Add(ttl))}
	token, err := store.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
	if err != nil {
		t.Fatalf("released acquire: %v", err)
	}
	if token.Owner != "standby" || token.Version != 8 {
		t.Fatalf("token=%+v want owner standby version 8", token)
	}
}

func TestAcquire_LegacyRowMayOmitOnlyObservationEvidence(t *testing.T) {
	const ttl = 20 * time.Second
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	row := validExistingLeaseRow(base, ttl)
	client := &observationMemoryClient{item: row}
	store := &Store{client: client, tableName: "leases", clk: clocktest.NewAt(base)}
	_, err := store.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
	if !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("complete legacy base tuple without observation evidence: %v", err)
	}
	if !persistedObservationPresent(client) {
		t.Fatal("legacy row did not initialize zero observation evidence")
	}
	if elapsed := persistedObservationElapsed(t, client); elapsed != 0 {
		t.Fatalf("legacy observation elapsed=%s want zero", elapsed)
	}
}

func TestAcquire_EmptyNewOwnerFailsBeforeWritingRow(t *testing.T) {
	client := &observationMemoryClient{}
	store := &Store{client: client, tableName: "leases", clk: clocktest.NewAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))}
	if _, err := store.Acquire(t.Context(), "lease-1", "", 20*time.Second, nil); !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("empty owner error=%v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.item != nil {
		t.Fatalf("empty owner wrote lease row: %v", client.item)
	}
}

func TestCurrent_CorruptExistingLeaseRowFailsClosed(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	row := validExistingLeaseRow(base, 20*time.Second)
	delete(row, attrVersion)
	store := &Store{client: &observationMemoryClient{item: row}, tableName: "leases", clk: clocktest.NewAt(base)}
	if _, err := store.Current(t.Context(), "lease-1"); !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("Current corrupt row error=%v", err)
	}
}
