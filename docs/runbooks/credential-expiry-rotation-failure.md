# Runbook: Credential Expiry / Rotation Failure

**Applies to:** any transport whose credentials resolve from a secret store
(SSM on the shipped AWS image; `file://` on the reference binary).
**Audience:** on-call operators.
**Risk:** low to act — the resolver serves the last-known-good credential through
a transient backend blip; the work is fixing the source.

## Symptom

- Brokers reject connections after a secret rotation; logs show
  `TEMPORARY_AUTH_FAILURE` or `NOT_AUTHORIZED`.
- `CredentialRefreshFailures` or `CredentialResolveFailure` climb.
- `CredentialStaleServed` is non-zero — the resolver is serving an expired
  last-known-good credential because the backend is unreachable.

## Diagnosis

1. Separate "rotation not applied" from "new credential rejected". The metrics
   under `GoBridge/Runtime` split cleanly
   ([monitoring.md#key-metrics](../aws-deployment/monitoring.md#key-metrics)):
   - `CredentialRotationApplied` — a rotation reached a live transport (success).
   - `CredentialRefreshFailures` — a resolve failed during the rotation poll.
   - `CredentialResolveFailure` (`code`) — a repository fetch failed, tagged with
     the error code so a permission denial (`NOT_AUTHORIZED`) is distinguishable
     from a backend outage (`UNAVAILABLE`).
   - `CredentialStaleServed` (`code`) — the resolver returned an expired
     credential after a retryable fetch error; a rising value means the secrets
     backend has been unreachable longer than the cache TTL.

2. Classify from the error code ([troubleshooting.md](../troubleshooting.md)):
   - `TEMPORARY_AUTH_FAILURE` — resolution succeeded but the broker reports
     "auth not yet propagated": a just-rotated credential not yet active, or
     clock skew. Usually clears within seconds
     ([troubleshooting.md#temporary_auth_failure](../troubleshooting.md#temporary_auth_failure)).
   - `CredentialResolveFailure{code=NOT_AUTHORIZED}` — the bridge's IAM role lost
     read access to the secret; `{code=UNAVAILABLE}` — the backend is down.

3. On the shipped AWS image, secrets resolve from SSM at startup and
   `admin_api_key_param` is mandatory
   (`deployment/aws-filebased-config/lib/model/bootstrap.go:123-125`,
   `deployment/aws-filebased-config/lib/bootstrap/secrets.go`). A rotation that
   never lands often means the SSM parameter was updated under a different name
   or the task role cannot read it.

## Action

- **`TEMPORARY_AUTH_FAILURE` that persists**: there is no HTTP "refresh"
  endpoint. The bridge already re-resolves on its rotation poll and reconnects on
  its own, so a persistent code means the broker keeps rejecting a credential that
  DID resolve -- usually clock skew or a value that is not yet active at the broker.
  Verify clock sync (NTP) on the bridge host, confirm the rotated credential is
  active at the broker/IdP, and if it outlasts the propagation window restart the
  process to force a fresh resolve and a clean reconnect
  ([troubleshooting.md#temporary_auth_failure](../troubleshooting.md#temporary_auth_failure)).
- **`CredentialResolveFailure{code=NOT_AUTHORIZED}`**: restore the IAM read grant
  (or the `file://` permissions) on the secret. Rotation resumes on the next poll.
- **`CredentialResolveFailure{code=UNAVAILABLE}` / rising `CredentialStaleServed`**:
  the backend is unreachable. The bridge keeps running on the last-known-good
  credential; fix the backend before the credential actually expires.
- **Rotation not applying at all**: confirm the new value is in the expected
  parameter/URI and that the resolver is polling. Enabling and observing rotation
  is documented in [Credential Rotation](../credentials-rotation.md#observing-rotation).

## Related runbooks

- [Broker outage / reconnect storm](broker-outage-reconnect-storm.md) — when the
  reconnect loop is driven by rejected credentials.
