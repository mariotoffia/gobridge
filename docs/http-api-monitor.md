# Monitor API Endpoints

The monitor server: unauthenticated health, liveness and readiness probes, and
the authenticated topology, route and deep-health endpoints. Split out of
[HTTP API](http-api.md), which covers configuration and the admin API.

Default listen address: `:8081`.

### Unauthenticated (health probes)

All health probes set `Cache-Control: no-cache, max-age=0` to prevent stale
caching by load balancers and CDNs.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/monitor/health` | Coarse health check -- returns **only** `{"status": ...}` |
| GET | `/api/v1/monitor/live` | Liveness probe (`{"status":"alive"}`, or `{"status":"terminal"}` + 503) |
| GET | `/api/v1/monitor/ready` | Readiness probe; optional `?level=` gate (`{"status":"ready","role":...}` or 503) |

The `health` endpoint is **unauthenticated** (for load balancers/orchestrators)
and therefore exposes only a coarse `status` string plus the HTTP status code --
never `instance_id`, route count, or component-failure detail, which are
reconnaissance and live behind auth on `/deephealth`. It returns HTTP 200 with
`{"status":"ok"}` when healthy, and HTTP 503 with `{"status": ...}` otherwise,
where `status` is one of `ok`, `unhealthy`, `not_running`, or `unavailable`
(runtime not wired).

The `ready` endpoint accepts an optional **`?level=`** query parameter that sets
how strict the gate is, least to most strict: `live`, `running`, `connected`,
`subscribed`, `full`. Without it, the bare probe requires the `full` readiness level -- HTTP 200
`{"status":"ready","role":...}` once every session is subscribed and every route
can dispatch, HTTP 503 `{"error":"not ready"}` otherwise. A `standby` instance is
capped at `subscribed` by design, so the bare probe answers 503 for it; use
`?level=connected` or `?level=subscribed` where a standby must count as healthy. With it, the response is
`{"status":..., "role":..., "level":..., "requested":...}` and returns HTTP 503
when the achieved `level` is below the `requested` one; an unknown level returns
HTTP 400. `role` is the failover role from lease ownership: `active`, `standby`,
or `standalone`. See
[Health checks and graceful shutdown](health-and-shutdown.md)
for probe-to-level mapping.

An instance that carries no routes and no sessions never rises above `running`,
so the legacy no-`level` probe answers 503 for it. This is deliberate: every
"all routes are ready" test is trivially satisfied when there are no routes, and
without the cap a bridge that started without a configuration would present
itself as a fully working member of the pool.

`live` reports the state of the **process**, not only of the runtime object it
can see. A composition root that wires its supervisor's terminal state into the
server answers 503 as soon as that supervisor is wedged -- a reconfiguration
swap and its recovery both failed, so there is no active runtime and nothing is
routed. Without that signal a wedged process is indistinguishable from a normal
swap window and would keep answering 200 forever.

### Authenticated

Sensitive endpoints require the monitor API key or fall back to the admin key
(admin access is a superset of monitor access).

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/monitor/topology` | Bridge topology (instance ID, running state, compact route list) |
| GET | `/api/v1/monitor/routes` | Route details with full policy (max_in_flight, ack_after, on_expired, on_perm_failure, on_filtered) |
| GET | `/api/v1/monitor/deephealth` | Deep health check (sessions, routes, service levels) |

### Topology response

```json
{
  "instance_id": "gobridge-1",
  "running": true,
  "routes": [
    { "id": "ingest", "delivery_mode": "direct_hold", "dispatch_mode": "single" }
  ]
}
```

### Monitor routes response

```json
{
  "routes": [
    {
      "id": "ingest",
      "delivery_mode": "direct_hold",
      "dispatch_mode": "single",
      "max_in_flight": 100,
      "max_replay": 3,
      "ack_after": "target_accept",
      "on_expired": "dlq",
      "on_perm_failure": "dlq",
      "on_filtered": "drop"
    }
  ]
}
```

### Deep health response

Returns HTTP 200 when ready for traffic, HTTP 503 otherwise. Covers runtime
state, session connectivity, lease status, and subscription convergence.

```json
{
  "running": true, "healthy": true,
  "instance_id": "gobridge-1", "role": "standalone",
  "ready_for_traffic": true, "empty": false, "service_level": "full",
  "level": "full",
  "sessions": [{
    "session_id": "mqtt-conn", "connected": true, "has_lease": false,
    "subscriptions_wanted": 2, "subscriptions_active": 2,
    "ready": true, "service_level": "full"
  }],
  "routes": [{ "id": "ingest", "delivery_mode": "direct_hold" }]
}
```

The `service_level` field aggregates across sessions: `full`, `degraded`, `none`.

The `empty` field is true when the instance carries no routes and no sessions,
so no message can be bridged through it. That is what a bridge started without
a usable configuration looks like -- for example when the configured file does
not exist and the process fell back to starting empty. An empty instance stays
alive (`/live` answers 200, so it is not restarted), but it never claims
`ready_for_traffic` and never reaches the `full` readiness level, so a load
balancer or a rollout gate does not steer traffic at a bridge that carries
none. Distinguish it from a bridge whose routes are merely still coming up:
both are not ready, but only the second one becomes ready on its own.

When the deep health response includes a `config_watch` object, its optional
`restart_required` field names a part of the desired configuration this process
has accepted and stored but cannot apply while running -- today, a changed
`http` block, because the listeners are bound once at startup. It stays absent
while the running process matches its desired configuration.
