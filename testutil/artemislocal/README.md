# artemislocal

Starts an Apache ActiveMQ Artemis container in Docker for integration tests.

## Docker image

`apache/activemq-artemis:latest-alpine`

**Exposed ports** (mapped to random free ports on 127.0.0.1):

| Container port | Protocol | Purpose |
|----------------|----------|---------|
| 5672 | AMQP 1.0 | Broker connections |
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
| `Credentials()` | Returns the configured username and password. |
| `Shutdown()` | Stops and removes the container. Safe to call multiple times. |
| `ForceStart(t)` | Resets state and starts a fresh container. Registers `t.Cleanup`. |
| `UniqueAddress(prefix)` | Returns an address name with a nanosecond timestamp suffix. |

## Configuration options

Pass options to `Configure()` before the first `Endpoint()` call.

| Option | Default | Description |
|--------|---------|-------------|
| `WithCleanOrphans(bool)` | `false` | Remove leftover `gobridge-artemis-*` containers on startup. |
| `WithImage(string)` | `apache/activemq-artemis:latest-alpine` | Override the Docker image. |
| `WithCredentials(user, pass)` | `admin` / `admin` | Override broker credentials. |
