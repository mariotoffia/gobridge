// Package asblocal provides shared test infrastructure for the Azure
// Service Bus emulator running in Docker.
//
// It manages two Docker containers on a shared network: an SQL Server
// backend and the Service Bus emulator. A pre-built Config.json is
// mounted into the emulator with test entities (queue + topic with
// subscriptions).
//
// Usage in test files:
//
//	func TestMain(m *testing.M) {
//	    asblocal.Configure(asblocal.WithCleanOrphans(true))
//	    code := m.Run()
//	    asblocal.Shutdown()
//	    os.Exit(code)
//	}
//
//	func TestSomething(t *testing.T) {
//	    connStr := asblocal.ConnectionString(t)
//	    // ... create sender/receiver with connection string ...
//	}
//
// The containers are started on first call to [ConnectionString].
// If the ASB_CONNECTION_STRING environment variable is set, no
// containers are started and that connection string is used directly.
package asblocal

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
)

// Pre-defined entity names available in the emulator.
const (
	TestQueue            = "test-queue"
	TestTopic            = "test-topic"
	TestSubscriptionAll  = "sub-all"
	TestSubscriptionFilt = "sub-filtered"
)

const (
	containerPrefix = "gobridge-asblocal-"
	networkPrefix   = "gobridge-asbnet-"

	defaultSQLImage      = "mcr.microsoft.com/mssql/server:2022-latest"
	defaultEmulatorImage = "mcr.microsoft.com/azure-messaging/servicebus-emulator:latest"

	sqlPassword = "Str0ngPa$$w0rd!"
)

type options struct {
	cleanOrphans  bool
	sqlImage      string
	emulatorImage string
}

var (
	mu           sync.Mutex
	resolved     bool
	fromEnv      bool
	connStr      string
	emuContainer string
	sqlContainer string
	configPath   string
	cleanupFn    func()
	initErr      error
	opts         options
)

// Option configures the ASB emulator test infrastructure.
type Option func(*options)

// WithCleanOrphans enables removal of all leftover gobridge-asblocal-*
// containers and gobridge-asbnet-* networks before starting new ones.
func WithCleanOrphans(enabled bool) Option {
	return func(o *options) { o.cleanOrphans = enabled }
}

// WithSQLImage overrides the SQL Server Docker image.
func WithSQLImage(image string) Option {
	return func(o *options) { o.sqlImage = image }
}

// WithEmulatorImage overrides the Service Bus emulator Docker image.
func WithEmulatorImage(image string) Option {
	return func(o *options) { o.emulatorImage = image }
}

// Configure applies options before the containers are started.
// Must be called before the first [ConnectionString] call.
func Configure(fns ...Option) {
	mu.Lock()
	defer mu.Unlock()
	if resolved {
		return
	}
	for _, fn := range fns {
		fn(&opts)
	}
}

// ConnectionString returns the Service Bus emulator connection string.
//
// On first call it checks ASB_CONNECTION_STRING; if unset it starts a
// SQL Server + Service Bus emulator pair in Docker. The test is skipped
// when -short is set or Docker is unavailable.
//
// Call [Shutdown] in TestMain after m.Run() to stop the containers.
func ConnectionString(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping ASB integration test in short mode")
	}

	mu.Lock()
	defer mu.Unlock()

	if !resolved {
		resolved = true
		if cs := os.Getenv("ASB_CONNECTION_STRING"); cs != "" {
			connStr = cs
			fromEnv = true
		} else {
			connStr, cleanupFn, initErr = startContainers()
		}
	} else if !fromEnv && emuContainer != "" {
		if !dockerexec.IsRunning(emuContainer) {
			t.Logf("asblocal: emulator container %s died, restarting...", emuContainer)
			// Capture why it died before cleanup removes the container.
			dumpDiagnostics()
			if cleanupFn != nil {
				cleanupFn()
			}
			connStr, cleanupFn, initErr = startContainers()
		}
	}

	if initErr != nil {
		t.Skipf("ASB emulator not available: %v", initErr)
	}
	return connStr
}

// Shutdown stops the emulator and SQL containers and removes the
// Docker network. Safe to call multiple times.
func Shutdown() {
	mu.Lock()
	defer mu.Unlock()
	if cleanupFn != nil {
		cleanupFn()
		cleanupFn = nil
	}
}

// UniqueQueue returns a queue name with a nanosecond timestamp suffix.
func UniqueQueue(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// WaitUntilReady verifies that the ASB emulator is accepting connections.
func WaitUntilReady(t testing.TB) {
	t.Helper()
	_ = ConnectionString(t)
}

func sqlImageName() string {
	if opts.sqlImage != "" {
		return opts.sqlImage
	}
	return defaultSQLImage
}

func emulatorImageName() string {
	if opts.emulatorImage != "" {
		return opts.emulatorImage
	}
	return defaultEmulatorImage
}

func startContainers() (string, func(), error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return "", nil, fmt.Errorf("docker not found: %w", err)
	}

	if opts.cleanOrphans {
		dockerexec.RemoveOrphans(containerPrefix)
		removeNetworks(networkPrefix)
	}

	amqpPort, err := dockerexec.FreePort()
	if err != nil {
		return "", nil, fmt.Errorf("find free AMQP port: %w", err)
	}
	httpPort, err := dockerexec.FreePort()
	if err != nil {
		return "", nil, fmt.Errorf("find free HTTP port: %w", err)
	}

	suffix := fmt.Sprintf("%d", amqpPort)
	netName := networkPrefix + suffix
	sqlName := containerPrefix + "sql-" + suffix
	emuName := containerPrefix + "emu-" + suffix
	// Recorded before anything starts so Diagnostics can dump either
	// container's logs no matter which startup gate fails.
	sqlContainer = sqlName

	_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", sqlName)
	_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", emuName)
	_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "network", "rm", netName)

	cleanup := func() {
		_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", emuName)
		_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "rm", "-f", sqlName)
		_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "network", "rm", netName)
		if configPath != "" {
			_ = os.Remove(configPath)
		}
	}

	// Create Docker network.
	out, err := dockerexec.Run(dockerexec.ExecTimeout, "network", "create", netName)
	if err != nil {
		return "", nil, fmt.Errorf("docker network create: %w\n%s", err, out)
	}

	// Write Config.json.
	cfgFile, err := os.CreateTemp("", "asblocal-config-*.json")
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("create temp config: %w", err)
	}
	if _, err := cfgFile.WriteString(configJSON()); err != nil {
		_ = cfgFile.Close()
		cleanup()
		return "", nil, fmt.Errorf("write config: %w", err)
	}
	_ = cfgFile.Close()
	configPath = cfgFile.Name()

	// Start SQL Server.
	out, err = dockerexec.Run(dockerexec.RunTimeout, "run", "-d",
		"--name", sqlName,
		"--network", netName,
		"--network-alias", "sqledge",
		"-e", "ACCEPT_EULA=Y",
		"-e", "MSSQL_SA_PASSWORD="+sqlPassword,
		sqlImageName(),
	)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("docker run sql: %w\n%s", err, out)
	}

	if err := dockerexec.WaitHealthy(sqlName, 30*time.Second); err != nil {
		dockerexec.LogFailure(sqlName)
		cleanup()
		return "", nil, fmt.Errorf("sql container: %w", err)
	}

	// Deterministic SQL readiness gate: SQL Server prints "Recovery is
	// complete" exactly when it starts accepting logins. The SQL port is not
	// published to the host (emulator reaches it over the docker network), so
	// the container's own log line is the observable state — no fixed sleep.
	if err := dockerexec.WaitLogLine(sqlName, "Recovery is complete", 90*time.Second); err != nil {
		dockerexec.LogFailure(sqlName)
		cleanup()
		return "", nil, fmt.Errorf("sql server: %w", err)
	}

	// Start Service Bus Emulator.
	out, err = dockerexec.Run(dockerexec.RunTimeout, "run", "-d",
		"--name", emuName,
		"--network", netName,
		"-p", fmt.Sprintf("127.0.0.1:%d:5672", amqpPort),
		"-p", fmt.Sprintf("127.0.0.1:%d:5300", httpPort),
		"-v", configPath+":/ServiceBus_Emulator/ConfigFiles/Config.json",
		"-e", "SQL_SERVER=sqledge",
		"-e", "MSSQL_SA_PASSWORD="+sqlPassword,
		"-e", "ACCEPT_EULA=Y",
		emulatorImageName(),
	)
	if err != nil {
		dockerexec.LogFailure(sqlName)
		cleanup()
		return "", nil, fmt.Errorf("docker run emulator: %w\n%s", err, out)
	}

	emuContainer = emuName

	if err := dockerexec.WaitHealthy(emuName, 30*time.Second); err != nil {
		dockerexec.LogFailure(emuName)
		dockerexec.LogFailure(sqlName)
		cleanup()
		return "", nil, fmt.Errorf("emulator container: %w", err)
	}

	if err := dockerexec.WaitTCP(amqpPort, 120*time.Second); err != nil {
		dockerexec.LogFailure(emuName)
		cleanup()
		return "", nil, fmt.Errorf("emulator AMQP: %w", err)
	}

	// TCP-level stabilize only: the emulator has no probeable readiness
	// endpoint here; AMQP protocol truth is gated by the consumer's real
	// send/receive warmup roundtrip (see servicebus integration TestMain).
	if err := dockerexec.StabilizeTCP(amqpPort); err != nil {
		dockerexec.LogFailure(emuName)
		cleanup()
		return "", nil, fmt.Errorf("emulator stabilization: %w", err)
	}

	cs := fmt.Sprintf(
		"Endpoint=sb://localhost:%d;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=SAS_KEY_VALUE;UseDevelopmentEmulator=true;",
		amqpPort,
	)

	return cs, cleanup, nil
}

func configJSON() string {
	return `{
  "UserConfig": {
    "Namespaces": [
      {
        "Name": "sbemulatorns",
        "Queues": [
          {
            "Name": "test-queue",
            "Properties": {
              "DeadLetteringOnMessageExpiration": false,
              "DefaultMessageTimeToLive": "PT1H",
              "DuplicateDetectionHistoryTimeWindow": "PT20S",
              "ForwardDeadLetteredMessagesTo": "",
              "ForwardTo": "",
              "LockDuration": "PT1M",
              "MaxDeliveryCount": 3,
              "RequiresDuplicateDetection": false,
              "RequiresSession": false
            }
          }
        ],
        "Topics": [
          {
            "Name": "test-topic",
            "Properties": {
              "DefaultMessageTimeToLive": "PT1H",
              "DuplicateDetectionHistoryTimeWindow": "PT20S",
              "RequiresDuplicateDetection": false
            },
            "Subscriptions": [
              {
                "Name": "sub-all",
                "Properties": {
                  "DeadLetteringOnMessageExpiration": false,
                  "DefaultMessageTimeToLive": "PT1H",
                  "LockDuration": "PT1M",
                  "MaxDeliveryCount": 3,
                  "ForwardDeadLetteredMessagesTo": "",
                  "ForwardTo": "",
                  "RequiresSession": false
                }
              },
              {
                "Name": "sub-filtered",
                "Properties": {
                  "DeadLetteringOnMessageExpiration": false,
                  "DefaultMessageTimeToLive": "PT1H",
                  "LockDuration": "PT1M",
                  "MaxDeliveryCount": 3,
                  "ForwardDeadLetteredMessagesTo": "",
                  "ForwardTo": "",
                  "RequiresSession": false
                },
                "Rules": [
                  {
                    "Name": "filter-by-env",
                    "Properties": {
                      "FilterType": "Correlation",
                      "CorrelationFilter": {
                        "Properties": {
                          "env": "test"
                        }
                      }
                    }
                  }
                ]
              }
            ]
          }
        ]
      }
    ],
    "Logging": {
      "Type": "File"
    }
  }
}`
}

// Diagnostics dumps emulator and SQL container state plus recent container
// logs to stderr.
//
// The emulator can pass every container-level readiness gate and still never
// serve AMQP: `docker run -p` makes docker-proxy bind the published host port
// immediately, so a TCP dial succeeds while the broker inside the container is
// absent or dead. When that happens the only evidence of the cause lives in
// the container's own log, which no failure path currently captures. Call this
// whenever a readiness or warmup gate gives up.
func Diagnostics() {
	mu.Lock()
	defer mu.Unlock()
	dumpDiagnostics()
}

// dumpDiagnostics is the unlocked form, for callers already holding mu.
func dumpDiagnostics() {
	for _, name := range []string{emuContainer, sqlContainer} {
		if name == "" {
			continue
		}
		out, err := dockerexec.Run(dockerexec.InspectTimeout, "inspect",
			"--format", "Running={{.State.Running}} ExitCode={{.State.ExitCode}} OOMKilled={{.State.OOMKilled}} Error={{.State.Error}}",
			name)
		if err == nil {
			fmt.Fprintf(os.Stderr, "--- docker inspect %s ---\n%s\n", name, out)
		} else {
			fmt.Fprintf(os.Stderr, "--- docker inspect %s failed: %v ---\n", name, err)
		}
		dockerexec.LogFailure(name)
	}
}

func removeNetworks(prefix string) {
	out, err := dockerexec.Run(dockerexec.InspectTimeout, "network", "ls", "-q",
		"--filter", "name="+prefix)
	if err != nil || len(out) == 0 {
		return
	}
	ids := strings.Fields(strings.TrimSpace(string(out)))
	for _, id := range ids {
		_, _ = dockerexec.Run(dockerexec.RemoveTimeout, "network", "rm", id)
	}
}
