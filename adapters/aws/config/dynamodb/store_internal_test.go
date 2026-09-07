package dynamodb

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/ports/configstoretest"
)

// TestConfigStoreConformanceUnit verifies the contract without Docker.
// DynamoDB's actual condition semantics are covered by the Local harness.
func TestConfigStoreConformanceUnit(t *testing.T) {
	configstoretest.Run(t, func(t *testing.T) ports.ConfigStore {
		t.Helper()
		return newCASLoader(&casFakeDDB{getReturnsVersion: -1})
	})
}

// TestSaveIfVersionUsesExpectedVersion verifies CAS needs no read and stamps both versions.
func TestSaveIfVersionUsesExpectedVersion(t *testing.T) {
	for _, expected := range []int{4, math.MaxInt/2 + 1, math.MaxInt - 1} {
		f := &casFakeDDB{
			hasRow: true, storedVersion: int64(expected), getReturnsVersion: -1,
			getErr: errors.New("unexpected read"),
		}
		l := newCASLoader(f)
		cfg := &ports.BridgeConfig{Version: 99, Bridge: ports.BridgeSettings{ID: "config-store"}}
		if err := l.SaveIfVersion(t.Context(), cfg, expected); err != nil {
			t.Fatal(err)
		}
		if f.getCalls != 0 || f.putCalls != 1 {
			t.Fatalf("SDK calls: reads=%d writes=%d, want 0 and 1", f.getCalls, f.putCalls)
		}
		stored, err := parser.Parse(strings.NewReader(f.storedData), parser.FormatJSON, ports.NewRegistry())
		if err != nil {
			t.Fatal(err)
		}
		want := expected + 1
		if cfg.Version != want || stored.Version != want || f.storedVersion != int64(want) || l.lastVersion != int64(want) {
			t.Errorf("versions: caller=%d JSON=%d row=%d cursor=%d, want %d",
				cfg.Version, stored.Version, f.storedVersion, l.lastVersion, want)
		}
		f.getErr = nil
		got, err := l.Load(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if got.Version != want {
			t.Errorf("Load.Version=%d, want %d", got.Version, want)
		}
	}
}

// TestLoadUsesStoredVersion verifies CAS baselines come from the row, not a stale JSON field.
func TestLoadUsesStoredVersion(t *testing.T) {
	for _, version := range []int64{0, 7} {
		f := &casFakeDDB{
			hasRow: true, storedVersion: version, versionAbsent: version == 0,
			storedData: `{"version":99,"bridge":{"id":"config-store"}}`, getReturnsVersion: -1,
		}
		got, err := newCASLoader(f).Load(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if got.Version != int(version) {
			t.Fatalf("Load.Version = %d, want row version %d", got.Version, version)
		}
	}
}

// TestConfigStoreRejectsInvalidVersions verifies malformed and exhausted counters fail closed.
func TestConfigStoreRejectsInvalidVersions(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  ddbtypes.AttributeValue
	}{
		{"negative", &ddbtypes.AttributeValueMemberN{Value: "-1"}},
		{"fractional", &ddbtypes.AttributeValueMemberN{Value: "1.5"}},
		{"overflow", &ddbtypes.AttributeValueMemberN{Value: "9223372036854775808"}},
		{"wrong_type", &ddbtypes.AttributeValueMemberS{Value: "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &casFakeDDB{hasRow: true, rawVersion: tc.raw, getReturnsVersion: -1}
			l := newCASLoader(f)
			if _, err := l.Load(t.Context()); !errors.Is(err, shared.ErrInvalidConfig) {
				t.Fatalf("Load error = %v, want ErrInvalidConfig", err)
			}
			cfg := &ports.BridgeConfig{Version: 42}
			if err := l.Save(t.Context(), cfg); !errors.Is(err, shared.ErrInvalidConfig) {
				t.Fatalf("Save error = %v, want ErrInvalidConfig", err)
			}
			if cfg.Version != 42 || f.putCalls != 0 {
				t.Fatalf("invalid stored version must not write or change caller: version=%d writes=%d", cfg.Version, f.putCalls)
			}
		})
	}
	for _, version := range []int{-1, math.MaxInt} {
		f := &casFakeDDB{hasRow: true, storedVersion: int64(version), getReturnsVersion: -1}
		l := newCASLoader(f)
		cfg := &ports.BridgeConfig{Version: 42}
		if err := l.SaveIfVersion(t.Context(), cfg, version); !errors.Is(err, shared.ErrInvalidConfig) {
			t.Fatalf("SaveIfVersion(%d) error = %v, want ErrInvalidConfig", version, err)
		}
		if err := l.Save(t.Context(), cfg); !errors.Is(err, shared.ErrInvalidConfig) {
			t.Fatalf("Save at %d error = %v, want ErrInvalidConfig", version, err)
		}
		if cfg.Version != 42 || f.putCalls != 0 {
			t.Fatalf("invalid version must not write or change caller: version=%d writes=%d", cfg.Version, f.putCalls)
		}
	}
}

// TestConfigStoreWriteFailures verifies both save paths preserve caller and watch versions.
func TestConfigStoreWriteFailures(t *testing.T) {
	writeErr := errors.New("write unavailable")
	for _, conditional := range []bool{false, true} {
		for _, failure := range []error{writeErr, &ddbtypes.ConditionalCheckFailedException{}} {
			f := &casFakeDDB{
				hasRow: true, storedVersion: 4, getReturnsVersion: -1, putErr: failure,
			}
			l := newCASLoader(f)
			l.lastVersion = 4
			cfg := &ports.BridgeConfig{Version: 99}
			var err error
			if conditional {
				err = l.SaveIfVersion(t.Context(), cfg, 4)
			} else {
				err = l.Save(t.Context(), cfg)
			}
			want := failure
			if isConditionFailed(failure) {
				want = shared.ErrVersionMismatch
			}
			if !errors.Is(err, want) {
				t.Fatalf("save conditional=%v: error=%v, want %v", conditional, err, want)
			}
			if cfg.Version != 99 || l.lastVersion != 4 || f.storedVersion != 4 {
				t.Fatalf("failed save changed versions: caller=%d cursor=%d row=%d", cfg.Version, l.lastVersion, f.storedVersion)
			}
		}
	}
}

// TestConfigStoreSaveReadFailure verifies a failed version read never becomes a first write.
func TestConfigStoreSaveReadFailure(t *testing.T) {
	readErr := errors.New("read unavailable")
	f := &casFakeDDB{getErr: readErr}
	cfg := &ports.BridgeConfig{Version: 42}
	if err := newCASLoader(f).Save(t.Context(), cfg); !errors.Is(err, readErr) {
		t.Fatalf("Save error = %v, want read error", err)
	}
	if f.putCalls != 0 || cfg.Version != 42 {
		t.Fatalf("failed read changed state: writes=%d caller=%d", f.putCalls, cfg.Version)
	}
}

// TestSaveIfVersionRejectsInvalidInput verifies preflight failures do not reach DynamoDB.
func TestSaveIfVersionRejectsInvalidInput(t *testing.T) {
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		cfg  *ports.BridgeConfig
		want error
	}{
		{"nil", t.Context(), nil, shared.ErrInvalidConfig},
		{"cancelled", cancelled, &ports.BridgeConfig{}, context.Canceled},
		{"oversized", t.Context(), &ports.BridgeConfig{
			Bridge: ports.BridgeSettings{ID: strings.Repeat("x", maxConfigItemBytes+1)},
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &casFakeDDB{getReturnsVersion: -1}
			err := newCASLoader(f).SaveIfVersion(tc.ctx, tc.cfg, 0)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("SaveIfVersion error = %v, want %v", err, tc.want)
			}
			if f.getCalls != 0 || f.putCalls != 0 {
				t.Fatalf("invalid input reached SDK: reads=%d writes=%d", f.getCalls, f.putCalls)
			}
			if tc.cfg != nil && tc.cfg.Version != 0 {
				t.Fatalf("failed save changed caller version to %d", tc.cfg.Version)
			}
		})
	}
}
