//go:build integration_aws || integration_local
// +build integration_aws integration_local

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"testing"

	ha "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/constructs/gobridgedynamodbha"
)

// How a deployed static member-slot cohort is observed and driven, independent
// of where it runs.
//
// The credentialed proof reaches a task over the sandbox VPC by its ENI address;
// the local proof reaches it through the emulator's own container network. That
// is the ONLY difference between the two runs, so it is the only thing a caller
// supplies: everything below — what a healthy slot looks like, what convergence
// means, what a commit does — is shared, so the two proofs cannot drift into
// asserting different things about the same protocol.

const (
	staticSlotControlID = "gobridge-ha-control"
	staticSlotWorkerA   = "gobridge-ha-worker-1"
	staticSlotWorkerB   = "gobridge-ha-worker-2"
)

// slotAdminPort and slotMonitorPort are the listeners a deployed member exposes:
// the admin config transaction API and the monitor deep-health endpoint.
const (
	slotAdminPort   = 8080
	slotMonitorPort = 8081
)

func staticSlotRoster() *ha.MemberSlots {
	return &ha.MemberSlots{
		ControlMemberID: staticSlotControlID,
		WorkerMemberIDs: []string{staticSlotWorkerA, staticSlotWorkerB},
	}
}

// cohortMember is one running task of the cohort, with the host its listeners
// answer on.
type cohortMember struct {
	TaskARN string
	Service string
	Host    string
}

// cohortProbe is the pair of capabilities a deployed cohort proof needs and
// cannot get the same way in both environments: enumerating the running tasks,
// and issuing an HTTP call that reaches one of them.
type cohortProbe struct {
	// Members lists the tasks that are running right now. A task that is not up
	// yet, or has no reachable address yet, is simply absent.
	Members func(ctx context.Context) ([]cohortMember, error)
	// Call issues one HTTP request and returns (status, body). It must return an
	// error only when the request could not be made at all.
	Call func(ctx context.Context, method, url string, header map[string]string, body []byte) (int, []byte, error)
}

// slotHealth is the part of one member's deep health this proof reads.
type slotHealth struct {
	MemberID       string `json:"member_id"`
	Generation     uint64 `json:"generation"`
	State          string `json:"state"`
	Applied        bool   `json:"applied"`
	ConfirmPending bool   `json:"confirm_pending"`
	// Stale and LastError are read FIRST by every predicate below. Every other
	// field is a projection of this member's last successful read of the rollout
	// row, so a stale block describes the cohort as it WAS. Without this a member
	// that permanently lost its view keeps reporting the generation it last saw,
	// and satisfies any wait for that generation forever.
	Stale              bool   `json:"stale"`
	LastError          string `json:"last_error"`
	BaselineGeneration uint64 `json:"baseline_generation"`
	BaselineDigest     string `json:"baseline_digest"`
	TerminalReason     string `json:"terminal_reason"`
	// Reason carries the barrier's own account of an abort or a nack, and
	// Acked/Nacked name who voted which way. Without them a failed rollout phase
	// reports a state and nothing about how the cohort reached it — and "nobody
	// answered" and "somebody refused" are different defects.
	Reason string   `json:"reason"`
	Acked  []string `json:"acked"`
	Nacked []string `json:"nacked"`
}

// fresh reports whether this observation may be believed at all.
func (h slotHealth) fresh() bool { return !h.Stale }

func slotURL(host string, port int, path string) string {
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + path
}

// commitLogLevel drives one live-safe config change through the admin config
// transaction API on the control slot. log_level is the smallest change the
// barrier classifies live-safe, so the proof is about the protocol rather than
// about what changed.
func commitLogLevel(t *testing.T, ctx context.Context, probe cohortProbe, controlHost, adminKey, level string) {
	t.Helper()
	commitOverlay(t, ctx, probe, controlHost, adminKey,
		map[string]any{"bridge": map[string]any{"log_level": level}})
}

// commitOverlay drives one config overlay through create, patch and commit on
// the control slot's admin API.
func commitOverlay(
	t *testing.T,
	ctx context.Context,
	probe cohortProbe,
	controlHost, adminKey string,
	overlay map[string]any,
) {
	t.Helper()
	base := slotURL(controlHost, slotAdminPort, "/api/v1/admin/config/transactions")

	var created struct {
		TxnID string `json:"txn_id"`
	}
	adminCall(t, ctx, probe, http.MethodPost, base, adminKey, nil, &created)
	if created.TxnID == "" {
		t.Fatal("config transaction create returned no txn_id")
	}
	adminCall(t, ctx, probe, http.MethodPatch, base+"/"+created.TxnID, adminKey, overlay, nil)

	// A coordinated cohort DEFERS: the commit is proposed to the barrier, so the
	// admin layer reports committed_not_applied rather than a completed swap. Both
	// that and a plain 200 are success here; only a rollback is a failure.
	var committed struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	status := adminCallStatus(t, ctx, probe, http.MethodPost, base+"/"+created.TxnID+"/commit", adminKey, nil, &committed)
	if committed.Status == "rolled_back" {
		t.Fatalf("commit rolled back: %s", committed.Error)
	}
	if status >= 400 && committed.Status != "committed_not_applied" {
		t.Fatalf("commit status=%d body status=%q error=%q", status, committed.Status, committed.Error)
	}
}

// observeSlots reads deep health from every running task and keys the rollout
// block by the member_id that task announces. A task that is not up yet, or has
// no rollout block, is simply absent.
//
// It also returns how many tasks ANSWERED, which the caller compares against the
// number of distinct ids: keying by member_id would otherwise hide the single
// worst deployment defect this profile can have — two running tasks under one
// identity, which makes the roster look satisfied while one seat is unoccupied.
func observeSlots(ctx context.Context, probe cohortProbe, adminKey string) (map[string]slotHealth, int) {
	out := map[string]slotHealth{}
	answered := 0
	members, err := probe.Members(ctx)
	if err != nil {
		return out, 0
	}
	for _, member := range members {
		if member.Host == "" {
			continue
		}
		health, ok := readSlotHealth(ctx, probe, member.Host, adminKey)
		if !ok || health.MemberID == "" {
			continue
		}
		answered++
		out[health.MemberID] = health
	}
	return out, answered
}

func readSlotHealth(ctx context.Context, probe cohortProbe, host, adminKey string) (slotHealth, bool) {
	status, body, err := probe.Call(ctx, http.MethodGet,
		slotURL(host, slotMonitorPort, "/api/v1/monitor/deephealth"),
		map[string]string{"X-API-Key": adminKey}, nil)
	if err != nil {
		return slotHealth{}, false
	}
	// Deep health answers 503 with the FULL body whenever the member is not
	// ready for traffic — which a member mid-swap, and possibly a warm standby,
	// simply is. Reading only 200 would confuse "not traffic-ready" with "not
	// there", and this proof is reading the rollout block, not readiness.
	if status != http.StatusOK && status != http.StatusServiceUnavailable {
		return slotHealth{}, false
	}
	var parsed struct {
		ConfigWatch struct {
			Rollout *slotHealth `json:"rollout"`
		} `json:"config_watch"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.ConfigWatch.Rollout == nil {
		return slotHealth{}, false
	}
	return *parsed.ConfigWatch.Rollout, true
}

// memberHost returns the host of the running task announcing the given member id.
func memberHost(ctx context.Context, probe cohortProbe, adminKey, memberID string) (string, error) {
	members, err := probe.Members(ctx)
	if err != nil {
		return "", err
	}
	for _, member := range members {
		if member.Host == "" {
			continue
		}
		if health, ok := readSlotHealth(ctx, probe, member.Host, adminKey); ok && health.MemberID == memberID {
			return member.Host, nil
		}
	}
	return "", fmt.Errorf("no running task announces member_id %q", memberID)
}

// memberTaskARN returns the task ARN of the running task announcing the given
// member id.
func memberTaskARN(ctx context.Context, probe cohortProbe, adminKey, memberID string) (string, error) {
	members, err := probe.Members(ctx)
	if err != nil {
		return "", err
	}
	for _, member := range members {
		if member.Host == "" {
			continue
		}
		if health, ok := readSlotHealth(ctx, probe, member.Host, adminKey); ok && health.MemberID == memberID {
			return member.TaskARN, nil
		}
	}
	return "", fmt.Errorf("no running task announces member_id %q", memberID)
}

func adminCall(t *testing.T, ctx context.Context, probe cohortProbe, method, url, adminKey string, body, out any) {
	t.Helper()
	if status := adminCallStatus(t, ctx, probe, method, url, adminKey, body, out); status >= 400 {
		t.Fatalf("%s %s returned %d", method, url, status)
	}
}

func adminCallStatus(
	t *testing.T,
	ctx context.Context,
	probe cohortProbe,
	method, url, adminKey string,
	body, out any,
) int {
	t.Helper()
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s body: %v", method, err)
		}
		payload = encoded
	}
	status, raw, err := probe.Call(ctx, method, url, map[string]string{
		"X-API-Key":    adminKey,
		"Content-Type": "application/json",
	}, payload)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	if out != nil {
		_ = json.NewDecoder(bytes.NewReader(raw)).Decode(out)
	}
	return status
}

// rolloutIsSettled reports whether a rollout state is one the barrier cannot
// move on from. "proposed", "staging" and "committed" are all states a rollout
// passes THROUGH on its way to being applied, so a cohort sitting in one of them
// has decided nothing yet.
func rolloutIsSettled(state string) bool {
	return state == rolloutStateAborted || state == rolloutStateReverted
}

// The two outcomes in which a proposal ends without being applied: the cohort
// never agreed to it, or it agreed and then took it back.
const (
	rolloutStateAborted  = "aborted"
	rolloutStateReverted = "reverted"
)

func keysOf(m map[string]slotHealth) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
