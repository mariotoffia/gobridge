//go:build longrunning

package longrunning_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/sqslocal"
)

// ---------------------------------------------------------------------------
// testInfra — per-test fresh Docker infrastructure
// ---------------------------------------------------------------------------

// testInfra holds the endpoints for the three local Docker services that
// were (re)started by withFreshInfra. Tests use the singleton helpers
// (sqslocal.Client, mqttlocal.BrokerURL, ddblocal.Client) for actual
// connections; these fields are informational / for logging.
type testInfra struct {
	SQSEndpoint string
	DDBEndpoint string
	MQTTBroker  string
}

// withFreshInfra kills all orphaned gobridge containers, then force-starts
// fresh SQS, DynamoDB, and MQTT containers. The containers are cleaned up
// automatically via t.Cleanup when the test finishes.
//
// Call this at the top of every long-running test to guarantee a clean,
// healthy infrastructure baseline.
func withFreshInfra(t *testing.T) *testInfra {
	t.Helper()

	killAllGobridgeContainers(t)

	sqsEP := sqslocal.ForceStart(t)
	ddbEP := ddblocal.ForceStart(t)
	mqttEP := mqttlocal.ForceStart(t)

	t.Logf("withFreshInfra: SQS=%s  DDB=%s  MQTT=%s", sqsEP, ddbEP, mqttEP)

	return &testInfra{
		SQSEndpoint: sqsEP,
		DDBEndpoint: ddbEP,
		MQTTBroker:  mqttEP,
	}
}

// newMQTTSession creates an MQTT session WITHOUT starting it. Use this
// when the runtime's SessionManager will manage the session lifecycle
// (exclusive sessions). The runtime calls session.Start() itself.
func newMQTTSession(
	t *testing.T, clientID string, mode connectivity.SessionMode,
) *paho.Session {
	t.Helper()
	url := mqttlocal.BrokerURL(t)
	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       clientID,
		KeepAlive:      30,
		ConnectTimeout: 15 * time.Second,
		CleanStart:     mode == connectivity.SessionEphemeral,
		ReceiveMaximum: 65534,
	}, mode, testLogger(t))

	t.Cleanup(func() { _ = sess.Close(context.Background()) })
	return sess
}

// killAllGobridgeContainers removes all Docker containers whose names match
// any of the known gobridge prefixes. This prevents stale containers from
// interfering with fresh infrastructure.
func killAllGobridgeContainers(t *testing.T) {
	t.Helper()

	prefixes := []string{
		"gobridge-sqslocal-",
		"gobridge-ddblocal-",
		"gobridge-mqtt-",
		"gobridge-mqttinst-",
		"ddb-local-",
	}

	for _, prefix := range prefixes {
		out, err := exec.Command(
			"docker", "ps", "-aq", "--filter", "name="+prefix,
		).Output()
		if err != nil {
			t.Logf("killAllGobridgeContainers: docker ps for %q: %v", prefix, err)
			continue
		}

		ids := strings.TrimSpace(string(out))
		if ids == "" {
			continue
		}

		// Split on newlines — each line is a container ID.
		containerIDs := strings.Fields(ids)
		args := append([]string{"rm", "-f"}, containerIDs...)

		rmOut, rmErr := exec.Command("docker", args...).CombinedOutput()
		if rmErr != nil {
			t.Logf("killAllGobridgeContainers: docker rm -f %q: %v\n%s",
				prefix, rmErr, rmOut)
		}
	}
}
