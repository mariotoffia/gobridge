package persistence_test

import (
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// The committed-config artifact is the durable last-committed configuration the
// cohort agreed on: the bytes a (re)joining member boots on and a member that
// missed a commit reconciles to. Generation 0 is the pre-rollout baseline seed,
// so it is a VALID generation (not a corruption) — the seed is what closes the
// restart-into-window and restart-after-abort residuals.
func TestCommittedRolloutConfig_ValidateAcceptsBaselineSeed(t *testing.T) {
	c := persistence.CommittedRolloutConfig{
		Generation:    0,
		ConfigVersion: 7,
		ConfigBytes:   []byte("bridge_id: b\nversion: 7\n"),
		Digest:        "cafef00d",
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("baseline seed must validate: %v", err)
	}
}

func TestCommittedRolloutConfig_ValidateAcceptsCommittedGeneration(t *testing.T) {
	c := persistence.CommittedRolloutConfig{
		Generation:    3,
		ConfigVersion: 42,
		ConfigBytes:   []byte("bridge_id: b\nversion: 42\n"),
		Digest:        "deadbeef",
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("committed generation must validate: %v", err)
	}
}

// Empty bytes cannot be a config to boot on — fail closed so a truncated durable
// row never yields an empty artifact a member would build nothing from.
func TestCommittedRolloutConfig_ValidateRejectsEmptyBytes(t *testing.T) {
	c := persistence.CommittedRolloutConfig{Generation: 3, ConfigVersion: 42, Digest: "deadbeef"}
	err := c.Validate()
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("empty bytes: err = %v, want ErrInvalidConfig", err)
	}
}

// A negative version is nonsense the version-gated boot resolution compares
// against — reject it rather than persist an unusable artifact.
func TestCommittedRolloutConfig_ValidateRejectsNegativeVersion(t *testing.T) {
	c := persistence.CommittedRolloutConfig{Generation: 3, ConfigVersion: -1, ConfigBytes: []byte("x"), Digest: "d"}
	err := c.Validate()
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("negative version: err = %v, want ErrInvalidConfig", err)
	}
}

// An empty digest leaves a reader no way to verify the bytes it fetched are the
// committed artifact (the reader recomputes and compares), so it is invalid.
func TestCommittedRolloutConfig_ValidateRejectsEmptyDigest(t *testing.T) {
	c := persistence.CommittedRolloutConfig{Generation: 3, ConfigVersion: 42, ConfigBytes: []byte("x")}
	err := c.Validate()
	if !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("empty digest: err = %v, want ErrInvalidConfig", err)
	}
}
