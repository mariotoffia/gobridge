---
applyTo: "adapters/http/**"
---

# HTTP message transport

Adds to `adapters.instructions.md`. Sources: ADR-0008 and
`docs/transports/http.md`. The admin and monitor API is `httpapi/`, which has
its own file.

- `Idempotency-Key`, `X-Dedup-Id` and `X-Ordering-Key` are lifted into
  `EnvelopeInput`. HTTP ingress never orders, even with `X-Ordering-Key`.
- The dedup LRU (`dedup_window`) records a key only after success, so a retry
  after a 5xx is processed again. A rejected message returns 400 and records
  nothing; `MESSAGE_FILTERED` returns 200 and records the key.
- Dispatch is detached but bounded: emit on `context.WithoutCancel` and always
  arm `max_dispatch_duration`. Without the bound a wedged downstream leaks a
  goroutine per request.
- A request that arrives before the receiver is ready gets an immediate 503.
  It is never parked.
- `X-Bridge-Forwarded` is trusted only with an `X-Bridge-Forward-Token` checked
  in constant time. A forwarded request for a route this node does not own gets
  `508` and is never forwarded again. A `3xx` is never followed; it is
  permanent. `Retry-After` is clamped to 30s.
- `api_key` and the forward token are different secrets, and an inline
  `api_key` is at least 16 characters.
- SSE with zero subscribers makes `Send` return a transient error unless
  `at_most_once_accept_loss` is set. An empty `redirect_endpoint` means 503,
  never a leaked peer endpoint.
- Auto IDs have the form `http-<instance-entropy>-<unixnano>-<counter>`, with
  the entropy from `crypto/rand`.
