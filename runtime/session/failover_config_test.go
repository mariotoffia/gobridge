package session

import (
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/shared"
)

func TestConfigEffectiveFailoverLeaseTimingUsesManagerDerivations(t *testing.T) {
	cfg := Config{SessionID: "s", Exclusive: true, LeaseTTL: 20 * time.Second, RenewInterval: 4 * time.Second, AcquirePollInterval: 3 * time.Second, RenewCallTimeout: 2 * time.Second}
	ttl, poll, renew := cfg.EffectiveFailoverLeaseTiming()
	if ttl != 20*time.Second || poll != 3*time.Second || renew != 2*time.Second {
		t.Fatalf("timing=%s,%s,%s", ttl, poll, renew)
	}
}

func TestConfigValidateFailoverFields(t *testing.T) {
	cfg := DefaultConfig("s", true)
	cfg.FailoverSLO = -time.Nanosecond
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative SLO accepted")
	}
	cfg = DefaultConfig("s", true)
	cfg.StartupAllowance = -time.Nanosecond
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative startup allowance accepted")
	}
	cfg = DefaultConfig("s", true)
	cfg.StartupAllowance = MaxStartupAllowance + time.Nanosecond
	if err := cfg.Validate(); !errors.Is(err, shared.ErrInvalidConfig) {
		t.Fatalf("unbounded startup allowance=%v", err)
	}
}
