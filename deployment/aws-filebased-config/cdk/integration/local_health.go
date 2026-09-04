//go:build integration_local
// +build integration_local

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Reading a deployed member's own account of itself.
//
// The cohort proof reads only the rollout block, because that is the only part
// of deep health the barrier is about. Everything else here — is it carrying
// traffic, which config version is it running — is what the data-plane and
// shared-config proofs assert on, and it comes from the same endpoint.

// deployedHealth is the part of a member's deep health these proofs read.
type deployedHealth struct {
	Running         bool `json:"running"`
	Healthy         bool `json:"healthy"`
	ReadyForTraffic bool `json:"ready_for_traffic"`
	// Empty distinguishes a bridge that came up on NO configuration from one
	// whose routes are merely still starting. Both answer 503; only one of them
	// will ever become ready, so a proof that accepted "not ready" without
	// reading this would wait out its whole budget on a member that had already
	// failed.
	Empty bool   `json:"empty"`
	Role  string `json:"role"`
	// Sessions and Routes are the member's own account of what it runs: which
	// sessions are connected and which of them hold a lease, and which delivery
	// mode each route is on. A shape proof reads these rather than inferring the
	// deployed configuration from the document it seeded.
	Sessions []struct {
		SessionID string `json:"session_id"`
		Connected bool   `json:"connected"`
		HasLease  bool   `json:"has_lease"`
	} `json:"sessions"`
	Routes []struct {
		ID           string `json:"id"`
		DeliveryMode string `json:"delivery_mode"`
		Ready        bool   `json:"ready"`
	} `json:"routes"`
	ConfigWatch struct {
		Degraded       bool   `json:"degraded"`
		Reason         string `json:"reason,omitempty"`
		RunningVersion *int   `json:"running_version,omitempty"`
		DesiredVersion *int   `json:"desired_version,omitempty"`
		LastApplyError string `json:"last_apply_error,omitempty"`
	} `json:"config_watch"`
}

// DeepHealth reads one member's deep health.
//
// A 503 is a valid answer with a full body — a member that is up but not
// traffic-ready returns exactly that — so only a transport failure or an
// unreadable body is an error here.
func (s LocalStack) DeepHealth(ctx context.Context, host string) (deployedHealth, error) {
	status, body, err := s.Call(ctx, http.MethodGet,
		slotURL(host, slotMonitorPort, "/api/v1/monitor/deephealth"),
		map[string]string{"X-API-Key": s.AdminKey}, nil)
	if err != nil {
		return deployedHealth{}, err
	}
	if status != http.StatusOK && status != http.StatusServiceUnavailable {
		return deployedHealth{}, fmt.Errorf("deep health answered %d: %s", status, truncateBody(body))
	}
	var health deployedHealth
	if err := json.Unmarshal(body, &health); err != nil {
		return deployedHealth{}, fmt.Errorf("parse deep health: %w (%s)", err, truncateBody(body))
	}
	return health, nil
}

// RunningConfigVersion is the config version one member is currently running,
// or -1 when it reports none.
func (s LocalStack) RunningConfigVersion(t *testing.T, ctx context.Context, host string) int {
	t.Helper()
	health, err := s.DeepHealth(ctx, host)
	if err != nil {
		t.Fatalf("read the running config version of %s: %v", host, err)
	}
	if health.ConfigWatch.RunningVersion == nil {
		return -1
	}
	return *health.ConfigWatch.RunningVersion
}

// WaitConfigVersionPast waits until a member reports running a config version
// strictly newer than after, and returns it.
func (s LocalStack) WaitConfigVersionPast(
	t *testing.T,
	ctx context.Context,
	host string,
	after int,
	timeout time.Duration,
) int {
	t.Helper()
	observed := -1
	err := pollUntil(ctx, 2*time.Second, timeout, func() (bool, error) {
		health, err := s.DeepHealth(ctx, host)
		if err != nil || health.ConfigWatch.RunningVersion == nil {
			return false, nil
		}
		observed = *health.ConfigWatch.RunningVersion
		return observed > after, nil
	})
	if err != nil {
		t.Fatalf("member %s never moved past config version %d (last saw %d): %v",
			host, after, observed, err)
	}
	return observed
}

// adminProbe adapts a deployed stack to the admin-call helpers the cohort proof
// already uses, so a config transaction is driven exactly one way in this suite.
//
// Members refuses rather than returning nothing: these helpers issue calls
// against a host the caller already resolved, and a topology without a cohort
// has no roster to enumerate — so a caller that reaches for one is asking the
// wrong probe and should be told, not handed an empty answer that reads as
// "no members are running".
func (s LocalStack) adminProbe() cohortProbe {
	return cohortProbe{
		Call: s.Call,
		Members: func(context.Context) ([]cohortMember, error) {
			return nil, fmt.Errorf("this stack has no cohort roster to enumerate")
		},
	}
}
