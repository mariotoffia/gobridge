# HTTP Transport

> Part of the [Transport Configuration Reference](../transport-configuration.md).

**Transport name:** `http`
**Factory:** `httptransport.NewFactory(opts...)`
**Capabilities:** `http_endpoint`
**OpenAPI spec:** `spec/http-adapter/http-api.yaml`

The HTTP transport exposes receivers as POST endpoints and senders as
Server-Sent Events (SSE) GET endpoints. All endpoints are mounted on an
internal `http.ServeMux` accessible via `factory.Handler()`.

```mermaid
flowchart LR
    Client1["HTTP Client"] -->|"POST /receivers/{id}/messages"| Recv["Receiver Handler"]
    Recv --> Bridge["Bridge Pipeline"]
    Bridge --> SSE["SSE Sender"]
    SSE -->|"GET /senders/{id}/events"| Client2["SSE Client"]
```

## Authentication

Both receivers and senders support optional per-endpoint API key
authentication. When `api_key` is configured, requests must include the key
via `X-API-Key` header or `Authorization: Bearer` token. Keys are compared
using SHA-256 constant-time comparison to prevent timing and length-based
information leaks. A `401` always carries an RFC 7235
`WWW-Authenticate: Bearer` challenge.

**Minimum key length (breaking).** An inline `api_key` shorter than 16
characters is rejected at decode time (`minAPIKeyLength`). A short key an
earlier build accepted must be lengthened to >= 16 characters.
Credential-resolved keys (via `credentials_uri`) are validated at the
credential layer and not re-checked here.

> **The `api_key` and the cluster forward token MUST be distinct secrets.**
> Every client presents `api_key` on each request; reusing it as the forward
> token would let any authenticated caller spoof `X-Bridge-Forwarded`. Provision
> two independent secrets.

## YAML Example

```yaml
receivers:
  - id: webhook-receiver
    transport: http
    options:
      path: "/transport/http/receivers/webhook-receiver/messages"
      api_key: "recv-secret-min-16ch"
      max_body_size: 1048576

senders:
  - id: event-stream
    transport: http
    options:
      mode: "sse"
      path: "/transport/http/senders/event-stream/events"
      heartbeat_interval: "30s"
      api_key: "sse-secret-min-16ch"
      max_clients: 100
```

## Receiver Options Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `path` | string | `/transport/http/receivers/{id}/messages` | POST endpoint path (literal mount point; no ServeMux `{}` metacharacters) |
| `api_key` | string | -- | Per-receiver API key (constant-time comparison; inline keys >= 16 chars) |
| `max_body_size` | int | 1048576 (1 MiB) | Maximum request body in bytes; a breach returns `413` |
| `dedup_window` | int | 4096 | Size of the node-local ingress idempotency LRU (remembered `Idempotency-Key` / `X-Dedup-Id` values) |
| `credentials_uri` | string | -- | URI resolved by the bridge credential store at build time (populates `api_key` when empty) |

## Receiver Request Format

The receiver accepts a single JSON POST value (trailing tokens are rejected with
`400`) with the following fields:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `subject` | string | **yes** | Logical event subject. Mapped 1:1 to `Envelope.Subject`. Not a topic or routing key. |
| `payload` | any JSON | no | Message content (stored as raw bytes) |
| `id` | string | no | Caller-provided message ID (auto-assigned a UUID when omitted) |
| `headers` | object | no | Custom metadata (reserved `x-bridge.*` keys stripped at ingress) |
| `expires_at` | RFC 3339 | no | Message TTL (drives `on_expired` policy) |

**First-class propagation headers.** The idempotency, dedup, and ordering keys
are accepted *only* through their dedicated non-reserved HTTP request headers and
re-stamped on the trusted side; a client cannot inject them via the reserved
`x-bridge.*` namespace (stripped at ingress):

| Header | Purpose |
|--------|---------|
| `Idempotency-Key` | Cross-hop identity/dedup key; feeds the ingress idempotency window and rides forwards |
| `X-Dedup-Id` | Alternative dedup key remembered by the ingress window |
| `X-Ordering-Key` | Propagated as envelope metadata for ordered *targets* (FIFO queues); HTTP ingress itself never orders |

## Sender Options Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `mode` | string | `sse` | Sender mode (only `sse` supported) |
| `path` | string | `/transport/http/senders/{id}/events` | GET endpoint path |
| `heartbeat_interval` | duration | `30s` | SSE keep-alive heartbeat interval |
| `write_timeout` | duration | `15s` | Per-frame SSE write deadline (re-armed before every frame; overrides a fronting server's global `WriteTimeout` and evicts a stalled subscriber) |
| `api_key` | string | -- | Per-sender API key (constant-time comparison; inline keys >= 16 chars) |
| `max_clients` | int | 10000 | Maximum concurrent SSE connections (no uncapped mode) |
| `redirect_endpoint` | string | -- (disabled) | `PeerInfo.Endpoints` key used to build a `307` redirect for a remote-owned route. Empty **disables** redirect (remote route → `503`) so an internal peer endpoint is never leaked to an SSE client. |
| `credentials_uri` | string | -- | URI resolved by the bridge credential store at build time |

## Resilience & Delivery Semantics

- **Content-Type validation.** HTTP ingress validates
  `Content-Type: application/json` when the header is present, returning
  `415 Unsupported Media Type` for non-JSON requests. Requests without a
  `Content-Type` header are accepted (assumed JSON).
- **Body limit → 413.** A body exceeding `max_body_size` returns
  `413 Request Entity Too Large`, distinct from the `400` used for malformed
  JSON or trailing tokens.
- **Automatic envelope ID.** HTTP ingress generates a unique UUID envelope ID
  when the request omits `id`, ensuring every message has a traceable
  identifier through the pipeline.
- **Ingress is at-least-once with a best-effort dedup window.** Each receiver
  keeps a bounded, node-local LRU of `Idempotency-Key` / `X-Dedup-Id` values of
  *successfully* processed requests (`dedup_window`, default 4096). A request
  presenting a remembered key is acknowledged `200` without re-emitting and
  counted on `HTTPIngressDuplicates`. Keys are recorded only after success, so a
  retry after a `5xx` is re-processed. The window is per-node/per-process and
  not persisted -- it bounds but does not eliminate duplicates.
- **Emit concurrency (deviation).** Unlike the default sequential emit contract,
  each in-flight request emits from its own handler goroutine. The downstream
  pipeline observes concurrent emits and **no ordering guarantee exists -- not
  even for requests sharing an `X-Ordering-Key`**. The ordering key is metadata
  for ordered targets only.
- **Dispatch decoupled from the client connection.** Once the body is converted
  to an envelope, the delivery is emitted on a `context.WithoutCancel` copy of
  the request context: a client disconnect cannot abort the pipeline
  mid-dispatch. The HTTP response still honours the client context (`504` on
  timeout/disconnect) -- a `504` means "outcome unknown", not "not processed".
  Producers that retry on `504` should supply `Idempotency-Key`.
- **Cluster forwarding is at-least-once.** A forward timeout after the peer
  received the body is retried as a fresh POST. Every forward carries an
  `Idempotency-Key` (the envelope's own or derived from its ID) so the peer's
  idempotency window absorbs the replay. Forward classification: `5xx`, `429`
  (`ErrThrottled`) and `408` (`ErrTimeout`) are transient and honour a bounded
  `Retry-After` hint (clamped to 30s); other `4xx` are permanent. An optional
  circuit breaker (`ForwarderConfig.Breaker`) makes a dead peer fail fast and
  emits `HTTPForwardBreakerOpen`.
- **Loop prevention.** A request carrying `X-Bridge-Forwarded: true` is trusted
  as an already-forwarded peer message (and processed locally) **only** when it
  also proves the shared `X-Bridge-Forward-Token` (constant-time compared). With
  no token configured the marker is never trusted, so a client cannot force
  local processing on a non-owner node. An already-forwarded request for a route
  this node does not own is refused with `508 Loop Detected`
  (`HTTPForwardLoopRefused`) -- neither processed nor re-forwarded -- so even an
  untokened cluster fails closed instead of entering an A->B->A loop.
- **SSE egress is at-most-once.** `Send` reports success even when zero
  subscribers are connected or every subscriber's buffer is full -- the route
  runner acks the source either way. Both zero-delivery cases are counted
  (`SSENoSubscribers` / `SSEAllDropped`) and logged. Stronger delivery requires
  fronting SSE egress with an outbox/DLQ route policy.
- **SSE frames carry no `id:` field.** Emitting one would make `EventSource`
  clients send `Last-Event-ID` on reconnect and expect a replay window that does
  not exist. The envelope ID remains in the JSON payload.
- **SSE egress header hygiene.** Internal-only reserved headers (`route-id`,
  `route-override`, `source-id`, `content-type`) are stripped before an envelope
  is serialised to a subscriber. Bridge-to-bridge propagated headers
  (correlation/causation/idempotency/tenant/trace/forwarded-*) and application
  headers pass through -- **if any SSE endpoint is publicly reachable, strip the
  `x-bridge.*` namespace plus `traceparent`/`tracestate` at the edge** or keep it
  internal, or that metadata leaks to external clients.
- **SSE cross-cluster redirect is opt-in.** A remote-owned route is refused with
  `503` by default; a `307` is emitted only when `redirect_endpoint` names a
  `PeerInfo.Endpoints` key, so the internal forwarder endpoint is never leaked in
  a `Location` header.
- **Forward error classification (route policy).** Forwarder responses map by
  status family: `5xx` → `ErrUnavailable` (transient, retriable), `4xx` →
  `ErrForwardFailed` (permanent, not retriable), enabling correct DLQ routing.
- **Graceful shutdown order.** `Factory.Close` fans out to every SSE sender,
  unblocking connected client handlers so `http.Server.Shutdown` can complete.
  **The composition root must call `Factory.Close` before server shutdown**;
  otherwise `Shutdown` blocks until clients disconnect on their own.

## Forwarder Configuration (`ForwarderConfig`)

Cluster forwarding is configured by the composition root, not the YAML
`options:` block. Defaults (`DefaultForwarderConfig`):

| Field | Default | Description |
|-------|---------|-------------|
| `Timeout` | `30s` | Per-forward request timeout |
| `IdleConnTimeout` | `90s` | Transport idle connection timeout |
| `MaxRetries` | `2` | Forward retry attempts |
| `RetryInitialDelay` | `100ms` | First retry backoff |
| `RetryMaxDelay` | `200ms` | Retry backoff ceiling |
| `MaxIdleConnsPerHost` | `32` | Transport idle conns per peer |
| `MaxConnsPerHost` | `64` | Transport max conns per peer |
| `ForwardToken` | -- | Shared secret sent as `X-Bridge-Forward-Token` (must match receiver `WithForwardToken`) |
| `ReceiverAPIKeys` | -- | Per-receiver-ID API keys used when forwarding to protected peers |
| `Breaker` | -- | Optional `ports.CircuitBreaker` gating each forward |
| `Metrics` | no-op | Receives `HTTPForwardBreakerOpen` |

## Cluster-Aware Routing

When a `RouteLocator` is configured, endpoints become cluster-aware:

- **Receivers**: If the target route is owned by a remote node, the message is
  transparently forwarded to the peer. The `X-Bridge-Forwarded: true` header
  prevents infinite loops.
- **Senders**: If the target route is remote, the client receives an HTTP 307
  redirect to the peer's SSE endpoint.

## Factory Options

The HTTP factory accepts functional options at registration time:

| Option | Description |
|--------|-------------|
| `WithPathPrefix(prefix)` | Override URL prefix (default `/transport/http`) |
| `WithRouteLocator(l)` | Set cluster-aware route locator |
| `WithMessageForwarder(fw)` | Set cluster message forwarder |
| `WithFactoryMetrics(m)` | Set metrics exporter |
| `WithFactoryLogger(l)` | Set structured logger |

---

