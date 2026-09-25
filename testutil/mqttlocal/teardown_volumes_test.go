package mqttlocal_test

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// The Mosquitto image declares /mosquitto/data and /mosquitto/log as volumes,
// so docker gives every broker container anonymous volumes of its own. The
// fixture's teardown must remove them with the container; otherwise every test
// run leaves more of them behind, with nothing using them.
//
// Category: integration (TESTS.md §1) — Docker-backed, skips in -short.

func TestBrokerInstance_TeardownRemovesAnonymousVolumes(t *testing.T) {
	if testing.Short() {
		t.Skip("Docker-backed broker fixture; skipped in -short")
	}
	if !dockerexec.DockerAvailable() {
		t.Skip("docker not found")
	}

	var volumes []string
	// The subtest owns the broker, so the fixture's own teardown has finished
	// by the time t.Run returns.
	if !t.Run("broker", func(t *testing.T) {
		broker := mqttlocal.NewBrokerInstance(t)
		out, err := dockerexec.Run(dockerexec.InspectTimeout, "inspect", "--format",
			`{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}} {{end}}{{end}}`,
			broker.ContainerName())
		if err != nil {
			t.Fatalf("inspect the broker's mounts: %v\n%s", err, out)
		}
		volumes = strings.Fields(string(out))
		if len(volumes) == 0 {
			t.Fatal("the broker has no anonymous volume, so this test proves nothing")
		}
	}) {
		return
	}

	// `docker volume inspect` fails for an absent volume, but also when the
	// daemon or the call itself fails, so its error proves nothing. Listing
	// succeeds whether or not the volumes exist; repeated name filters match
	// any of them.
	args := []string{"volume", "ls", "-q"}
	for _, volume := range volumes {
		args = append(args, "--filter", "name="+volume)
	}
	out, err := dockerexec.Run(dockerexec.InspectTimeout, args...)
	if err != nil {
		t.Fatalf("list volumes: %v\n%s", err, out)
	}
	remaining := make(map[string]bool)
	for name := range strings.FieldsSeq(string(out)) {
		remaining[name] = true
	}
	for _, volume := range volumes {
		if remaining[volume] {
			t.Errorf("volume %s outlived its broker", volume)
		}
	}
}
