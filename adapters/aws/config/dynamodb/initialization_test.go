package dynamodb

import (
	"errors"
	"testing"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func TestCreateIfAbsentPreservesExistingRows(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, versionless := range []bool{false, true} {
			f := &casFakeDDB{hasRow: existing, versionAbsent: versionless, storedData: "[", getReturnsVersion: -1}
			l := newCASLoader(f)
			cfg := &ports.BridgeConfig{Version: 99, Bridge: ports.BridgeSettings{ID: "seed"}}
			created, err := l.CreateIfAbsent(t.Context(), cfg)
			if err != nil || created == existing {
				t.Fatalf("existing=%v versionless=%v: created=%v err=%v", existing, versionless, created, err)
			}

			if existing {
				if f.storedData != "[" || cfg.Version != 99 {
					t.Fatal("existing row or candidate modified")
				}
			} else {
				got, err := l.Load(t.Context())
				if err != nil || got.Version != 1 || cfg.Version != 1 {
					t.Fatalf("first version: cfg=%v err=%v", got, err)
				}
			}
			if f.lastPutCond != "attribute_not_exists(#pk)" {
				t.Fatalf("not strict create-only: %s", f.lastPutCond)
			}
		}
	}
}

func TestCreateIfAbsentFailureIsNotAbsence(t *testing.T) {
	fault := &ddbtypes.ResourceNotFoundException{}
	f := &casFakeDDB{getErr: fault, putErr: fault}
	l := newCASLoader(f)
	_, err := l.Load(t.Context())
	if !errors.Is(err, fault) || errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("missing table must not mean missing document: %v", err)
	}
	cfg := &ports.BridgeConfig{Version: 99}
	created, err := l.CreateIfAbsent(t.Context(), cfg)
	if created || !errors.Is(err, fault) || cfg.Version != 99 {
		t.Fatalf("failed create: created=%v version=%d err=%v", created, cfg.Version, err)
	}
}
