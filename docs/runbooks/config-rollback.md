# Runbook: Config Rollback

**Applies to:** deployments with the admin config transactions API wired
(the reference binary and the shipped AWS image both wire it).
**Audience:** on-call operators.
**Risk:** medium — a commit changes the running config; a bad commit can degrade
delivery. The transaction flow is reversible, which is the point of this runbook.

## Symptom

- A config change just committed and errors followed (routes dropping, sessions
  failing, delivery stalling).
- A commit returned `committed_not_applied` or `rolled_back` and you need to know
  whether disk and the running runtime agree.
- The `ConfigDegraded` gauge flipped to `1` after a reload that reported success —
  the config **applied but has not converged** (MQTT deep-health routes
  `ConfigDegraded` incidents here).
- An in-progress transaction needs to be abandoned.

## Diagnosis

1. Read the current effective (redacted) config and its version
   ([http-api.md#config-transactions](../http-api-admin.md#config-transactions)):

   ```bash
   curl -s -H "X-API-Key: ${ADMIN_KEY}" \
     "http://<host>:8080/api/v1/admin/config" | jq .
   ```

2. Interpret the commit outcome you got back:
   - `{"status":"committed","version":N}` (200) — disk and runtime both updated.
   - `{"status":"committed_applying","version":N}` (202) — the durable write
     succeeded and the applier is still swapping (or the bridge is paused/shutting
     down and recorded it for a later resume). The write is **retained**; the
     runtime is converging. No rollback action — this is not a failure.
   - `{"status":"rolled_back","version":N}` (500) — the apply failed and the
     **previous on-disk config was restored**; a restart recovers the last good
     config. Disk is safe.
   - `{"status":"committed_not_applied","version":N}` (500) — the durable write
     succeeded but the in-band apply failed and could not be restored: **disk and
     the running runtime have diverged** and you must reconcile.

3. **After a `committed_applying` (202) — poll to resolution.** The applier
   returns one terminal signal and does **not** call you back when the swap
   later lands, so the resolution is read from deep health rather than waited
   for on the commit connection:

   ```bash
   curl -s -H "X-API-Key: ${ADMIN_KEY}" \
     "http://<host>:8080/api/v1/monitor/deephealth" \
     | jq '.config_watch | {reconfigure_pending, desired_version, running_version, last_apply_error}'
   ```

   | Reading | Meaning | Action |
   |---|---|---|
   | `reconfigure_pending: true`, `running_version` < `desired_version`, no `last_apply_error` | The swap is still in flight. | Wait and re-poll. |
   | `reconfigure_pending: false`, `running_version == desired_version` | The swap landed; the commit succeeded. | None. |
   | `reconfigure_pending: true` **and** `last_apply_error` set | The swap failed definitively; the runtime kept the **previous** config while disk holds the new one. | Reconcile as for `committed_not_applied` (Action below). |
   | `reconfigure_pending: true` with a `config_watch.rollout` block undecided | This member deliberately has not applied a config the cohort has not committed. | Follow [cluster config rollout](cluster-config-rollout.md), not this runbook. |

   Alarm threshold: a single instance holding `reconfigure_pending: true` with no
   `last_apply_error` for longer than the apply deadline plus one transport
   activation budget (60 s + activation; ~2 min in the shipped profile) is a
   stuck swap, not a slow one — treat it as `committed_not_applied`. A rollback
   fired against the in-flight window is the one dangerous move here: the
   runtime may still adopt the config you just reverted.

4. In a fleet sharing one config file, compare `config_version` across instances
   (surfaced on the monitor plane): an instance whose
   version lags the others has not yet converged.

5. **`ConfigDegraded == 1` — applied but not converged.** A reload reports
   success once the new runtime is *built and started*, but MQTT dials and
   reconciles in background goroutines. A syntactically-valid-but-broker-invalid
   config (denied credentials, an ACL-rejected topic filter) therefore commits
   as a **successful** reload while the transport never reaches broker truth.
   Past the transport's activation budget the post-swap convergence watch flips
   `ConfigDegraded` to `1`. Both the generic runtime **and** the shipped AWS
   bootstrap emit this signal (the bootstrap gained a post-swap convergence
   watch — RECONFIG-1). Read the reason from deep health:

   ```bash
   curl -s -H "X-API-Key: ${ADMIN_KEY}" \
     "http://<host>:8080/api/v1/monitor/deephealth" | jq '.config_watch.reason'
   ```

   Then distinguish the two cases:
   - **Slow convergence** (still catching up — a broker reconnect backoff, a
     lease still being acquired, sessions climbing toward `LevelSubscribed`):
     the config is valid; **wait** and re-check that sessions reach Full and
     `ConfigDegraded` clears on its own. No rollback.
   - **Genuinely broker-invalid config** (`config_watch.reason` cites an
     ACL/credential/subscription rejection and sessions never converge): the
     new config cannot reach broker truth. **Revert it** with the Action below —
     a valid config will not converge by waiting.

## Action

Roll a config change back with the same transactions API that committed it —
there is no separate rollback endpoint. Open → PATCH the previous values →
commit.

```bash
BASE="http://<host>:8080/api/v1/admin/config"

# 1. Open a transaction against the current version.
TXN=$(curl -s -X POST -H "X-API-Key: ${ADMIN_KEY}" \
  "${BASE}/transactions" | jq -r .txn_id)

# 2. PATCH the fields back to their prior values (JSON BridgeConfig overlay).
#    PATCH is merge-only: omitted/empty fields keep the current value, so send
#    the values you want restored.
curl -s -X PATCH -H "X-API-Key: ${ADMIN_KEY}" -H "Content-Type: application/json" \
  -d @previous-values.json "${BASE}/transactions/${TXN}" | jq .

# 3. Commit: validates, CAS-checks the version, writes to disk, applies.
curl -s -X POST -H "X-API-Key: ${ADMIN_KEY}" \
  "${BASE}/transactions/${TXN}/commit" | jq .
```

- **Abandon an in-progress transaction** (nothing has gone live yet): discard it.

  ```bash
  curl -s -X DELETE -H "X-API-Key: ${ADMIN_KEY}" "${BASE}/transactions/${TXN}"
  # → {"status":"rolled_back"}
  ```

- **`409` on commit** — a concurrent write moved the version since you opened the
  transaction. Re-open against the new version and re-apply.
- **`422` on commit** — the merged config failed validation (`validation_errors`
  lists the fields) or would erase plugin options; fix the overlay and retry.
- **After a `committed_not_applied`** — reconcile deliberately: the committed
  version is on disk, so either restart the affected instance to load it, or open
  a new transaction to converge disk and runtime
  ([http-api.md#config-transactions](../http-api-admin.md#config-transactions)).

PATCH merge semantics (empty-string, empty-list, and `"[REDACTED]"` preserve the
current value; secrets and `session_id` cannot be cleared) are documented in
[http-api.md#config-transactions](../http-api-admin.md#config-transactions).

## Related

- Clustered staged rollout across control + workers:
  [CDK Scenario 5 — Staged config rollout](../scenarios/cdk/05-multi-bridge-cluster.md#staged-config-rollout).
