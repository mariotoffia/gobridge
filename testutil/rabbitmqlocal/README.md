# rabbitmqlocal

Starts a RabbitMQ container in Docker for integration tests.

## Docker image

`rabbitmq:management-alpine`

**Exposed ports** (mapped to random free ports on 127.0.0.1):

| Container port | Protocol | Purpose |
|----------------|----------|---------|
| 5672 | AMQP 0-9-1 | Broker connections |
| 15672 | HTTP | Management API |

## Environment variable

Set `RABBITMQ_URL` to skip the container and connect to an existing broker.

```bash
export RABBITMQ_URL="amqp://guest:guest@localhost:5672/"
```

## TestMain setup

```go
func TestMain(m *testing.M) {
    rabbitmqlocal.Configure(rabbitmqlocal.WithCleanOrphans(true))
    code := m.Run()
    rabbitmqlocal.Shutdown()
    os.Exit(code)
}

func TestSomething(t *testing.T) {
    ep := rabbitmqlocal.Endpoint(t)
    // use ep to create AMQP clients
}
```

Tests run with `-short` are skipped. If Docker is missing, the test is skipped too.

## Helper functions

| Function | Description |
|----------|-------------|
| `Endpoint(t)` | Returns the AMQP broker URL. Starts the container on first call. |
| `ManagementURL(t)` | Returns the HTTP management API URL. |
| `Shutdown()` | Stops and removes the container. Safe to call multiple times. |
| `ForceStart(t)` | Resets state and starts a fresh container. Registers `t.Cleanup`. |
| `UniqueQueue(prefix)` | Returns a queue name with a nanosecond timestamp suffix. |
| `UniqueExchange(prefix)` | Returns an exchange name with a nanosecond timestamp suffix. |
| `CreateQueue(t, name)` | Declares a queue via the management API. |
| `CreateExchange(t, name, kind)` | Declares an exchange via the management API. |
| `BindQueue(t, queue, exchange, routingKey)` | Binds a queue to an exchange. |

## Configuration options

Pass options to `Configure()` before the first `Endpoint()` call.

| Option | Default | Description |
|--------|---------|-------------|
| `WithCleanOrphans(bool)` | `false` | Remove leftover `gobridge-rabbitmq-*` containers on startup. |
| `WithImage(string)` | `rabbitmq:management-alpine` | Override the Docker image. |
| `WithCredentials(user, pass)` | `guest` / `guest` | Override broker credentials. |
