package bootstrap

import (
	"os"
	"testing"

	"github.com/mariotoffia/gobridge/testutil/ddblocal"
	"github.com/mariotoffia/gobridge/testutil/flocilocal"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// The integration tests here share one Mosquitto, one DynamoDB Local and one
// Floci container per test process. Nothing but Shutdown removes a shared
// container, so without this TestMain every run without -short left all three
// running. The orphan sweep removes the ones a killed run left behind.
func TestMain(m *testing.M) {
	ddblocal.Configure(ddblocal.WithCleanOrphans(true))
	flocilocal.Configure(flocilocal.WithCleanOrphans(true))
	mqttlocal.Configure(mqttlocal.WithCleanOrphans(true))

	code := m.Run()

	flocilocal.Shutdown()
	mqttlocal.Shutdown()
	ddblocal.Shutdown()
	os.Exit(code)
}
