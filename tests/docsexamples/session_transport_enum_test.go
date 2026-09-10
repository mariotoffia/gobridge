package docsexamples_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"

	"github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp091"
	"github.com/mariotoffia/gobridge/adapters/amqp/transport/amqp10"
	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/adapters/azure/transport/servicebus"
	httptransport "github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
)

// A `sessions[].transport` value is only meaningful on a transport whose factory
// reports ports.CapStatefulSession — every other factory returns a nil session
// and the builder then refuses any receiver or sender bound to it. The enum the
// configuration reference publishes for that key is therefore derived from the
// registered adapters and their capabilities, not typed by hand: a kind listed
// there that is stateless sends an operator to a config the builder rejects, and
// a stateful kind missing from it is one they cannot discover.
//
// Category: unit (TESTS.md §1) — the adapters are composed exactly as the
// binaries compose them; no network, no broker.

const sessionsReferenceHeading = "## `sessions`"

// transportAdapter pairs an adapter's registered kind names with its factory, so
// the stateful set is read from the same objects the builder consults.
type transportAdapter struct {
	register func(*ports.Registry) error
	factory  ports.TransportFactory
}

func registeredTransportAdapters(t *testing.T) []transportAdapter {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return []transportAdapter{
		{register: paho.Register, factory: paho.NewFactory(logger)},
		{register: amqp091.Register, factory: amqp091.NewFactory(logger)},
		{register: amqp10.Register, factory: amqp10.NewFactory(logger)},
		{register: sqs.Register, factory: sqs.NewFactory(logger)},
		{register: servicebus.Register, factory: servicebus.NewFactory(logger)},
		{register: httptransport.Register, factory: httptransport.NewFactory()},
	}
}

// statefulAndStatelessKinds splits every registered kind name (short and
// qualified alias alike) by whether its factory can create a session.
func statefulAndStatelessKinds(t *testing.T) (stateful, stateless []string) {
	t.Helper()
	for _, adapter := range registeredTransportAdapters(t) {
		reg := ports.NewRegistry()
		require.NoError(t, adapter.register(reg))
		kinds := reg.Kinds()
		require.NotEmpty(t, kinds, "an adapter registered no kinds")
		if slices.Contains(adapter.factory.Capabilities(), ports.CapStatefulSession) {
			stateful = append(stateful, kinds...)
		} else {
			stateless = append(stateless, kinds...)
		}
	}
	slices.Sort(stateful)
	slices.Sort(stateless)
	return stateful, stateless
}

// documentedSessionTransports returns every backticked token in the `transport`
// row of the sessions field table.
func documentedSessionTransports(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "configuration-reference.md"))
	require.NoError(t, err)

	token := regexp.MustCompile("`([a-z0-9._]+)`")
	inSection := false
	for line := range strings.SplitSeq(string(src), "\n") {
		if strings.HasPrefix(line, sessionsReferenceHeading) {
			inSection = true
			continue
		}
		if !inSection {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			break
		}
		if !strings.HasPrefix(line, "| `transport` |") {
			continue
		}
		var kinds []string
		for _, m := range token.FindAllStringSubmatch(strings.TrimPrefix(line, "| `transport` |"), -1) {
			kinds = append(kinds, m[1])
		}
		slices.Sort(kinds)
		return slices.Compact(kinds)
	}
	require.FailNow(t, "no `transport` row under the sessions heading of docs/configuration-reference.md")
	return nil
}

func TestSessionTransportEnum_MatchesStatefulRegistryCapabilities(t *testing.T) {
	stateful, stateless := statefulAndStatelessKinds(t)
	require.NotEmpty(t, stateful, "no registered transport reports a stateful session")

	documented := documentedSessionTransports(t)
	require.Equal(t, stateful, documented,
		"the sessions `transport` enum must list exactly the kinds whose factory reports CapStatefulSession")
	for _, kind := range stateless {
		require.NotContains(t, documented, kind, "%q creates no session and must not be offered as one", kind)
	}
}
