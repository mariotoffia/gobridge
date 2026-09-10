package docsexamples_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	nativestore "github.com/mariotoffia/gobridge/adapters/native/store"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/session"
)

// The published failover figures are DERIVED, not hand-maintained: the page
// states what the bridge itself discloses at startup for each of the two lease
// profiles a route session can inherit. Publishing only a lease TTL understated
// the enforced bound by more than 2x, and publishing only the standalone
// default hid the clustered profile entirely — an operator planning cluster
// recovery then used the wrong one.
//
// Category: unit (TESTS.md §1) — the builder opens nothing; the Markdown page
// is the fixture.

const failoverProfilePage = "docs/failover-budget.md"

const failoverProfileTable = "| Profile | `lease_ttl` |"

var failoverProfileSeconds = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)s`)

// brokerPathProbe is the threshold the probe blueprint declares. Any positive
// value works: the broker-path budget is linear in it, so subtracting it back
// out yields the fixed part the page publishes.
const brokerPathProbe = 60 * time.Second

func TestFailoverProfiles_PublishedBudgetsMatchWhatTheBridgeDiscloses(t *testing.T) {
	root := repoRoot(t)
	page := filepath.Join("docs", "failover-budget.md")
	src, err := os.ReadFile(filepath.Join(root, page))
	require.NoError(t, err)

	rows := failoverProfileRows(t, failoverProfilePage, string(src))
	require.Len(t, rows, 2, "%s must publish BOTH derived lease profiles", failoverProfilePage)

	for _, clustered := range []bool{false, true} {
		profile := "standalone"
		if clustered {
			profile = "clustered"
		}
		t.Run(profile, func(t *testing.T) {
			row, ok := rows[profile]
			require.Truef(t, ok, "%s publishes no %s profile row", failoverProfilePage, profile)

			cadence := routing.BaselineLeaseTiming(clustered, routing.LeaseTimingRequest{}).Resolve()
			requirePublished(t, profile, "lease_ttl", row.cadence[0], cadence.LeaseTTL)
			requirePublished(t, profile, "renew_interval", row.cadence[1], cadence.RenewInterval)
			requirePublished(t, profile, "renew_call_timeout", row.cadence[2], cadence.RenewCallTimeout)
			requirePublished(t, profile, "acquire_poll_interval", row.cadence[3], cadence.AcquirePollInterval)
			requirePublished(t, profile, "step_down_grace", row.cadence[4], publishedStepDownGrace(clustered))

			ownerDeath, brokerPath := discloseFailoverBudgets(t, clustered)
			requirePublished(t, profile, "owner-death budget", row.ownerDeath, ownerDeath)
			requirePublished(t, profile, "broker-path budget", row.brokerPathFixed, brokerPath-brokerPathProbe)
		})
	}
}

// publishedPrecision is what the page rounds to. A derived renew interval is
// not a whole number of nanoseconds, so an exact match would force the page to
// print "366.249999999s"; any real drift moves a budget by seconds, so nothing
// can hide under 10ms.
const publishedPrecision = 10 * time.Millisecond

func requirePublished(t *testing.T, profile, field string, published, computed time.Duration) {
	t.Helper()
	require.Equalf(t, computed.Round(publishedPrecision), published.Round(publishedPrecision),
		"%s publishes %s = %s for the %s profile; the code resolves %s",
		failoverProfilePage, field, published, profile, computed)
}

// publishedStepDownGrace is the grace each baseline carries. It is not part of
// the resolved lease cadence, so it comes from the profile constructor the
// builder uses.
func publishedStepDownGrace(clustered bool) time.Duration {
	if clustered {
		return session.HAConfig("profile", true).StepDownGrace
	}
	return session.DefaultConfig("profile", true).StepDownGrace
}

type failoverProfileRow struct {
	// cadence holds the five published lease columns in table order:
	// lease_ttl, renew_interval, renew_call_timeout, acquire_poll_interval,
	// step_down_grace.
	cadence         [5]time.Duration
	ownerDeath      time.Duration
	brokerPathFixed time.Duration
}

// failoverProfileRows reads the two published rows out of the profile table,
// keyed by the deployment mode each one names.
func failoverProfileRows(t *testing.T, page, src string) map[string]failoverProfileRow {
	t.Helper()
	lines := strings.Split(src, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), failoverProfileTable) {
			start = i
			break
		}
	}
	require.NotEqualf(t, -1, start, "%s has no derived-lease-profile table (header %q)", page, failoverProfileTable)

	rows := map[string]failoverProfileRow{}
	for _, line := range lines[start+2:] {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			break
		}
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		require.Lenf(t, cells, 8, "%s: malformed profile row %q", page, line)
		mode := "standalone"
		if strings.Contains(strings.ToLower(cells[0]), "clustered") {
			mode = "clustered"
		}
		row := failoverProfileRow{
			ownerDeath:      parsePublishedSeconds(t, page, cells[6]),
			brokerPathFixed: parsePublishedSeconds(t, page, cells[7]),
		}
		for i := range row.cadence {
			row.cadence[i] = parsePublishedSeconds(t, page, cells[i+1])
		}
		rows[mode] = row
	}
	return rows
}

func parsePublishedSeconds(t *testing.T, page, cell string) time.Duration {
	t.Helper()
	m := failoverProfileSeconds.FindStringSubmatch(cell)
	require.NotNilf(t, m, "%s: cell %q states no duration in seconds", page, cell)
	value, err := strconv.ParseFloat(m[1], 64)
	require.NoError(t, err)
	return time.Duration(value * float64(time.Second))
}

// discloseFailoverBudgets builds a blueprint at the SHIPPED defaults — no lease
// overrides, no failover_slo — and reads both budgets back out of the startup
// disclosure, so the page can only ever state what the bridge states.
func discloseFailoverBudgets(t *testing.T, clustered bool) (ownerDeath, brokerPath time.Duration) {
	t.Helper()
	mode := "standalone"
	if clustered {
		mode = "clustered"
	}
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "failover-profile", DeploymentMode: mode},
		Sessions: []ports.SessionDef{{
			ID: "ingress", Transport: "mqtt", SessionMode: string(connectivity.SessionEphemeral),
			Config: paho.Config{Session: paho.SessionOptions{
				BrokerURLs:     []string{"tcp://broker.example.test:1883"},
				ClientID:       "failover-profile-in",
				ClientIDSuffix: "hostname",
				CleanStart:     true,
			}},
		}, {
			// The lease-bearing session carries the SENDER only, so the probe
			// needs no managed-subscription store to answer a question about
			// lease timing.
			ID: "exclusive", Transport: "mqtt", SessionMode: string(connectivity.SessionExclusive),
			Config: paho.Config{Session: paho.SessionOptions{
				BrokerURLs: []string{"tcp://broker.example.test:1883"},
				ClientID:   "failover-profile",
			}},
		}},
		Receivers: []ports.ReceiverDef{{ID: "rx", Transport: "mqtt", SessionID: "ingress",
			Topics: []ports.SubscriptionDef{{Topic: "$share/profile/in", QoS: 1}}}},
		Senders:  []ports.SenderDef{{ID: "tx", Transport: "mqtt", SessionID: "exclusive"}},
		Bindings: []ports.BindingDef{{ID: "bind", SenderID: "tx", SessionID: "exclusive", Address: "profile/out"}},
		Routes: []ports.RouteDef{{
			ID: "route", ReceiverID: "rx", Bindings: []string{"bind"},
			DeliveryMode: "shared_outbox",
			Policy:       ports.PolicyDef{AckAfter: "outbox_persist"},
			Session: &ports.RouteSessionDef{
				SessionID: "exclusive", SenderID: "tx",
				BrokerHealthStepDown: brokerPathProbe.String(),
			},
		}},
	}
	// The clustered profile is refused on a process-local lease store, so both
	// probes use the same distributed stand-in the published-example builder
	// uses. Nothing is opened over a network and no lease is acquired: Plan
	// only constructs.
	dir := realTempDir(t)
	cfg.Stores.Lease = &ports.StoreConfig{Type: "dynamodb",
		Config: nativestore.SQLiteConfig{Path: filepath.Join(dir, "lease.db")}}
	cfg.Stores.Outbox = &ports.StoreConfig{Type: "dynamodb",
		Config: nativestore.SQLiteConfig{Path: filepath.Join(dir, "outbox.db")}}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	plan, err := bridge.NewBuilder(cfg,
		bridge.WithBlueprintValidator(config.Validate),
		bridge.WithLogger(logger)).
		RegisterTransportFactory("mqtt", paho.NewFactory(nil)).
		RegisterStoreFactory("dynamodb", distributedSQLiteStandIn{nativestore.NewSQLiteStoreFactory()}).
		Plan(t.Context())
	require.NoError(t, err, "profile probe blueprint")
	plan.Close()

	return disclosedDuration(t, buf.String(), "failover_budget"),
		disclosedDuration(t, buf.String(), "broker_path_failover_budget")
}

func disclosedDuration(t *testing.T, logged, key string) time.Duration {
	t.Helper()
	m := regexp.MustCompile(key + `=([0-9a-z.]+)`).FindStringSubmatch(logged)
	require.NotNilf(t, m, "startup disclosure carries no %s: %s", key, logged)
	d, err := time.ParseDuration(m[1])
	require.NoError(t, err)
	return d
}
