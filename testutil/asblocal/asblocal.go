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
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
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
	sqlContainer string
	emuContainer string
	networkName  string
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
		if !isContainerRunning(emuContainer) {
			t.Logf("asblocal: emulator container %s died, restarting...", emuContainer)
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
		removeOrphans(containerPrefix)
		removeNetworks(networkPrefix)
	}

	amqpPort, err := freePort()
	if err != nil {
		return "", nil, fmt.Errorf("find free AMQP port: %w", err)
	}
	httpPort, err := freePort()
	if err != nil {
		return "", nil, fmt.Errorf("find free HTTP port: %w", err)
	}

	suffix := fmt.Sprintf("%d", amqpPort)
	netName := networkPrefix + suffix
	sqlName := containerPrefix + "sql-" + suffix
	emuName := containerPrefix + "emu-" + suffix

	_ = exec.Command("docker", "rm", "-f", sqlName).Run()
	_ = exec.Command("docker", "rm", "-f", emuName).Run()
	_ = exec.Command("docker", "network", "rm", netName).Run()

	cleanup := func() {
		_ = exec.Command("docker", "rm", "-f", emuName).Run()
		_ = exec.Command("docker", "rm", "-f", sqlName).Run()
		_ = exec.Command("docker", "network", "rm", netName).Run()
		if configPath != "" {
			_ = os.Remove(configPath)
		}
	}

	// Create Docker network.
	out, err := exec.Command("docker", "network", "create", netName).CombinedOutput()
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
	out, err = exec.Command("docker", "run", "-d",
		"--name", sqlName,
		"--network", netName,
		"--network-alias", "sqledge",
		"-e", "ACCEPT_EULA=Y",
		"-e", "MSSQL_SA_PASSWORD="+sqlPassword,
		sqlImageName(),
	).CombinedOutput()
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("docker run sql: %w\n%s", err, out)
	}

	if err := waitForContainerHealthy(sqlName, 30*time.Second); err != nil {
		logContainerFailure(sqlName)
		cleanup()
		return "", nil, fmt.Errorf("sql container: %w", err)
	}

	// Give SQL a moment to finish initialization.
	time.Sleep(5 * time.Second)

	// Start Service Bus Emulator.
	out, err = exec.Command("docker", "run", "-d",
		"--name", emuName,
		"--network", netName,
		"-p", fmt.Sprintf("127.0.0.1:%d:5672", amqpPort),
		"-p", fmt.Sprintf("127.0.0.1:%d:5300", httpPort),
		"-v", configPath+":/ServiceBus_Emulator/ConfigFiles/Config.json",
		"-e", "SQL_SERVER=sqledge",
		"-e", "MSSQL_SA_PASSWORD="+sqlPassword,
		"-e", "ACCEPT_EULA=Y",
		emulatorImageName(),
	).CombinedOutput()
	if err != nil {
		logContainerFailure(sqlName)
		cleanup()
		return "", nil, fmt.Errorf("docker run emulator: %w\n%s", err, out)
	}

	sqlContainer = sqlName
	emuContainer = emuName
	networkName = netName

	if err := waitForContainerHealthy(emuName, 30*time.Second); err != nil {
		logContainerFailure(emuName)
		logContainerFailure(sqlName)
		cleanup()
		return "", nil, fmt.Errorf("emulator container: %w", err)
	}

	if err := waitForTCP(amqpPort, 120*time.Second); err != nil {
		logContainerFailure(emuName)
		cleanup()
		return "", nil, fmt.Errorf("emulator AMQP: %w", err)
	}

	if err := stabilize(amqpPort); err != nil {
		logContainerFailure(emuName)
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

func removeOrphans(prefix string) {
	out, err := exec.Command("docker", "ps", "-aq",
		"--filter", "name="+prefix).Output()
	if err != nil || len(out) == 0 {
		return
	}
	ids := strings.Fields(strings.TrimSpace(string(out)))
	if len(ids) > 0 {
		args := append([]string{"rm", "-f"}, ids...)
		_ = exec.Command("docker", args...).Run()
	}
}

func removeNetworks(prefix string) {
	out, err := exec.Command("docker", "network", "ls", "-q",
		"--filter", "name="+prefix).Output()
	if err != nil || len(out) == 0 {
		return
	}
	ids := strings.Fields(strings.TrimSpace(string(out)))
	for _, id := range ids {
		_ = exec.Command("docker", "network", "rm", id).Run()
	}
}

func isContainerRunning(name string) bool {
	out, err := exec.Command("docker", "inspect",
		"--format", "{{.State.Running}}", name).Output()
	return err == nil && len(out) > 0 && out[0] == 't'
}

func waitForContainerHealthy(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "inspect",
			"--format", "{{.State.Running}} {{.State.ExitCode}}", name).Output()
		if err == nil {
			s := strings.TrimSpace(string(out))
			if strings.HasPrefix(s, "true") {
				return nil
			}
			if strings.Contains(s, "false") {
				return fmt.Errorf("container %s exited (inspect: %s)", name, s)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("container %s did not reach running state within %v", name, timeout)
}

func waitForTCP(port int, timeout time.Duration) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	return fmt.Errorf("TCP connect to %s failed within %v: %v", addr, timeout, lastErr)
}

func stabilize(port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for i := 0; i < 3; i++ {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return err
		}
		_ = conn.Close()
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}

func logContainerFailure(name string) {
	out, _ := exec.Command("docker", "logs", "--tail", "50", name).CombinedOutput()
	if len(out) > 0 {
		fmt.Fprintf(os.Stderr, "--- docker logs %s ---\n%s\n--- end ---\n", name, out)
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}
