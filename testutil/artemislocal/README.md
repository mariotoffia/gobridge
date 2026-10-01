# artemislocal

Starts an Apache ActiveMQ Artemis container in Docker for integration tests.

## Docker image

`apache/activemq-artemis:2.44.0-alpine`, pinned by digest in `artemislocal.go`.

**Exposed ports** (mapped to random free ports on 127.0.0.1):

| Container port | Protocol | Purpose |
|----------------|----------|---------|
| 5672 | AMQP 1.0 | Broker connections |
| 5671 | AMQP 1.0 over TLS | Broker connections; only with `WithTLS()` |
| 8161 | HTTP | Web console |

## Environment variable

Set `ARTEMIS_URL` to skip the container and connect to an existing broker.

```bash
export ARTEMIS_URL="amqp://localhost:5672"
```

## TestMain setup

```go
func TestMain(m *testing.M) {
    artemislocal.Configure(artemislocal.WithCleanOrphans(true))
    code := m.Run()
    artemislocal.Shutdown()
    os.Exit(code)
}

func TestSomething(t *testing.T) {
    ep := artemislocal.Endpoint(t)
    // use ep to create AMQP 1.0 clients
}
```

Tests run with `-short` are skipped. If Docker is missing, the test is skipped too.

## Helper functions

| Function | Description |
|----------|-------------|
| `Endpoint(t)` | Returns the AMQP 1.0 broker URL. Starts the container on first call. |
| `ConsoleURL(t)` | Returns the web console URL. |
| `TLSEndpoint(t)` | Returns `amqps://127.0.0.1:<port>`. Empty without `WithTLS()`. |
| `CAPEM(t)` | Returns the PEM certificate of the authority that signed the broker's TLS certificate. Trust only this. Empty without `WithTLS()`. |
| `NetworkEndpoint(t)` | Returns `amqp://<container name>:5672`, the address a client in another container on the network uses. Empty without `WithNetwork(...)`. |
| `NetworkTLSEndpoint(t)` | Returns `amqps://<container name>:5671`. Empty unless both `WithTLS()` and `WithNetwork(...)` are set. |
| `Credentials()` | Returns the configured username and password. |
| `Shutdown()` | Stops and removes the container. Safe to call multiple times. |
| `ForceStart(t)` | Resets state and starts a fresh container. Registers `t.Cleanup`. |
| `UniqueAddress(prefix)` | Returns an address name with a nanosecond timestamp suffix. |

## Configuration options

Pass options to `Configure()` before the first `Endpoint()` call.

| Option | Default | Description |
|--------|---------|-------------|
| `WithCleanOrphans(bool)` | `false` | Remove leftover `gobridge-artemis-*` containers on startup. |
| `WithImage(string)` | `apache/activemq-artemis:2.44.0-alpine` (pinned by digest) | Override the Docker image. |
| `WithCredentials(user, pass)` | `admin` / `admin` | Override broker credentials. |
| `WithTLS()` | off | Add an AMQP 1.0 TLS listener. The helper generates a certificate authority and a server certificate valid for `localhost`, `127.0.0.1` and the container name. Startup waits until a TLS login that trusts only that authority succeeds. |
| `WithNetwork(name)` | none | Attach the container to an existing Docker network, so a client in another container reaches the broker by container name. Ports stay published on 127.0.0.1. You create the network before the broker starts and remove it after the broker is gone. |

The four address functions above also return an empty string when `ARTEMIS_URL`
points the tests at a broker this package did not start.

## Reaching the broker from another container

Inside a container, `127.0.0.1` is that container, not the host. A client there
dials the broker by container name on a shared Docker network:

```go
func TestFromAContainer(t *testing.T) {
    // Create the network first, so its removal runs after the broker's.
    network := fmt.Sprintf("gobridge-test-net-%d", time.Now().UnixNano())
    if out, err := dockerexec.Run(dockerexec.ExecTimeout, "network", "create", network); err != nil {
        t.Fatalf("%v\n%s", err, out)
    }
    t.Cleanup(func() { _, _ = dockerexec.Run(dockerexec.RemoveTimeout, "network", "rm", network) })

    artemislocal.Configure(artemislocal.WithTLS(), artemislocal.WithNetwork(network))
    artemislocal.ForceStart(t)

    inside := artemislocal.NetworkTLSEndpoint(t) // for the client in the other container
    outside := artemislocal.TLSEndpoint(t)       // for the test process
    ca := artemislocal.CAPEM(t)                  // both trust only this
    // ...
}
```
