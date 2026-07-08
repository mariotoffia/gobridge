package infra

import (
	"testing"
	"time"
)

func TestBootstrapConfig_EffectiveCredentialPollInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval string
		want     time.Duration
	}{
		{"empty uses default", "", DefaultCredentialPollInterval},
		{"valid duration", "30s", 30 * time.Second},
		{"invalid falls back", "nope", DefaultCredentialPollInterval},
		{"negative falls back", "-1m", DefaultCredentialPollInterval},
		{"zero falls back", "0s", DefaultCredentialPollInterval},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := BootstrapConfig{CredentialPollInterval: tc.interval}
			if got := c.EffectiveCredentialPollInterval(); got != tc.want {
				t.Errorf("EffectiveCredentialPollInterval() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBootstrapConfig_EffectiveCredentialPollJitter(t *testing.T) {
	tests := []struct {
		name     string
		interval string
		jitter   string
		want     time.Duration
	}{
		{"empty defaults to ~10% of interval", "", "", DefaultCredentialPollInterval / 10},
		{"empty jitter tracks custom interval", "10m", "", time.Minute},
		{"explicit jitter honored", "5m", "15s", 15 * time.Second},
		{"explicit zero disables jitter", "5m", "0s", 0},
		{"invalid jitter falls back to 10%", "10m", "bogus", time.Minute},
		{"negative jitter falls back to 10%", "10m", "-1s", time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := BootstrapConfig{
				CredentialPollInterval: tc.interval,
				CredentialPollJitter:   tc.jitter,
			}
			if got := c.EffectiveCredentialPollJitter(); got != tc.want {
				t.Errorf("EffectiveCredentialPollJitter() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBootstrapConfig_EffectiveCredentialEmitOnStart(t *testing.T) {
	truePtr := true
	falsePtr := false
	tests := []struct {
		name string
		val  *bool
		want bool
	}{
		{"nil defaults to true (Finding 1)", nil, true},
		{"explicit true", &truePtr, true},
		{"explicit false restores legacy", &falsePtr, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := BootstrapConfig{CredentialEmitOnStart: tc.val}
			if got := c.EffectiveCredentialEmitOnStart(); got != tc.want {
				t.Errorf("EffectiveCredentialEmitOnStart() = %v, want %v", got, tc.want)
			}
		})
	}
}
