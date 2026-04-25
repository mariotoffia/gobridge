# FIX_PLAN

Deferred work and known issues from the 2026-04-24 architecture investigation follow-up. Each item has enough context (files, symbols, rationale) to resume without re-reading the full session.

## Critical — Test Hangs

### 1. Pre-existing paho flake: `TestAnaMore_Sender_NilEnvelope_PanicsAsCallerBug`

- **File:** `adapters/mqtt/transport/paho/` (search the test name in `analysis_*_test.go` or `bug_*_test.go`).
- **Symptom:** Test hangs for the full 2-minute timeout; `make test` fails on this package only.
- **Reproducible on main** before any of this session's changes. Independent of P0-P3 work.
- **Likely cause:** Test sends a nil envelope expecting the sender to panic-as-caller-bug; somewhere the panic is recovered or the expected channel close never happens, so the test's wait-for-panic hangs.
- **Fix approach:**
  1. Run in isolation: `go test -count=1 -run TestAnaMore_Sender_NilEnvelope_PanicsAsCallerBug -timeout 10s ./adapters/mqtt/transport/paho/`
  2. Inspect what the test is waiting on — check the recover path in `paho/sender.go` or `paho/cb_sender.go`. If the sender now classifies the nil envelope as a `domain.BridgeError` instead of panicking, update the test assertion.
  3. Note: the `ports.Sender.Send` contract is ambiguous re: nil envelope handling — this may be a contract-clarification opportunity.

---

## P3 — Credential Rotation (pattern established, transports pending)

### 2. AMQP091 credential rotation

- **File to modify:** `adapters/amqp/transport/amqp091/session.go` (and siblings).
- **Pattern to follow:** `adapters/mqtt/transport/paho/session_credentials.go` (reference implementation).
- **Steps:**
  1. Add `liveCreds` field guarded by session mutex.
  2. Seed `liveCreds` in the session's `Start` method.
  3. Install a connection factory/reconnect hook that reads `liveCreds` on each reconnect.
  4. Implement `ApplyCredentials(ctx, domain.CredentialSet) error` that updates `liveCreds` and forces reconnect (AMQP091 has `Connection.Close` → reconnect loop).
  5. Make session implement `bridge.CredentialAware` interface (defined in `bridge/credential_refresh.go`).
- **Test:** mirror `adapters/mqtt/transport/paho/credentials_refresh_test.go` structure.

### 3. AMQP10 credential rotation

- **File:** `adapters/amqp/transport/amqp10/session.go`.
- **Same pattern as AMQP091.** AMQP10 uses `go-amqp` — check if it supports credential re-auth; if not, reconnect.

### 4. Azure Service Bus credential rotation

- **File:** `adapters/azure/transport/servicebus/session.go`.
- **ASB uses token-based auth (SAS or AAD).** Rotation is effectively token refresh — check if `azservicebus.ClientOptions` supports a token provider callback; if so, wiring is cleaner than full reconnect.

### 5. SQS credential rotation

- **File:** `adapters/aws/transport/sqs/`.
- **Easiest of the transports.** SQS is stateless per-request; swap the AWS SDK credentials provider on next request. No reconnect needed. Check `aws.Config.Credentials` and `credentials.NewStaticCredentialsProvider`.

### 6. Supervisor credential store — push variant missing

- **File:** `bridge/supervisor.go` — `WithSupervisorCredentialStore` currently accepts pull-only.
- **Fix:** add `WithSupervisorPushCredentialStore(ports.PushCredentialStore)` and `WithSupervisorPolledCredentialStore(ports.PullCredentialStore, ports.PollBasedWrapperConfig)`. Mirror the Builder API in `bridge/builder.go`.
- **Trivial — likely <50 LOC.**

### 7. TLS rotation not supported in `ApplyCredentials`

- **File:** `adapters/mqtt/transport/paho/session_credentials.go`.
- **Covered by:** `TestApplyCredentials_TLSIgnoredForNow`.
- **Why deferred:** autopaho's live `ConnectionManager` can't perform a new TLS handshake without full teardown. Supporting TLS material rotation needs a full session restart (close + new `autopaho.NewConnection`).
- **Fix approach:** add a `Reload()` or `Restart()` method on `Session` that does full teardown + re-Start. Called when `CredentialSet.TLSMaterial` differs from `liveCreds.TLSMaterial`. Document that TLS rotation is more expensive than password rotation.

### 8. Receiver/sender-level `credentials_uri` not wired to refresher

- **Current:** only session-level `credentials_uri` is watched by `bridge.CredentialRefresher`.
- **Impact:** for MQTT this is a non-issue (receivers/senders share session creds). For HTTP or per-endpoint transports, per-route credential rotation isn't observable.
- **Fix:** extend `CredentialRefresher` to iterate receiver/sender specs in addition to sessions. See `bridge/credential_refresh.go` — the watcher loop currently filters to session specs only.

---

## P3 — Outbox Drain Timeout

### 9. YAML config surface not extended

- **File:** `config/config.go` (or wherever `BridgeConfig` / `BridgeSettings` lives — grep `DrainTimeout`).
- **Current YAML:** `bridge.drain_timeout` maps to legacy `OutboxDrainerConfig.DrainTimeout`.
- **Fix:** add `bridge.per_record_drain_timeout` and `bridge.max_drain_timeout` to the YAML schema, wire through to `OutboxDrainerConfig.PerRecordDrainTimeout` and `OutboxDrainerConfig.MaxDrainTimeout`.
- **Touchpoints:**
  1. `config/config.go` — add fields with YAML tags.
  2. `bridge/builder_prepare.go` — forward the new fields from config into `OutboxDrainerConfig`.
  3. `docs/configuration-reference.md` — document the new fields.
  4. `validate/` — if validation rules apply (e.g., `MaxDrainTimeout > PerRecordDrainTimeout`).

### 10. Supervisor doesn't expose scaled drain timeout

- **File:** `bridge/supervisor.go` — `WithDefaultDrainTimeout` only sets legacy field.
- **Fix:** add `WithDefaultPerRecordDrainTimeout(time.Duration)` and `WithDefaultMaxDrainTimeout(time.Duration)` options. Trivial.

### 11. Scenario walkthroughs reference legacy `DrainTimeout`

- **Files:** `docs/scenarios/*.md` (14 scenario docs).
- **Fix:** `grep -rn "drain_timeout\|DrainTimeout" docs/` and update examples that would benefit from the scaled timeout. Mention that `DrainTimeout` is retained for backward compatibility but the scaled fields are preferred for production workloads.

---

## Known Code-Quality Issues (Lint Modernization)

All the following are modernization suggestions the IDE linter flags when files are touched. Pre-existing across most files; newly-introduced ones are flagged separately. Non-blocking — code works correctly.

### 12. `maps.Copy` / `maps.Clone` modernizations

Replace explicit `for k,v := range src { dst[k] = v }` loops with `maps.Copy(dst, src)` or `maps.Clone(src)`. Go 1.21+.

- `bridge/builder_resolve.go:47`
- `bridge/builder_prepare.go:219`
- `bridge/supervisor.go:384, 388, 392, 423`
- `runtime/bridge_health.go:38`
- `adapters/mqtt/transport/paho/session_reconcile.go:53`

### 13. `min`/`max` builtin modernizations

Replace `if a > b { a = b }` and similar with `a = min(a, b)` / `a = max(a, b)`. Go 1.21+.

- `runtime/session_manager.go:91, 96, 268` (and 513 pre-P3)
- `runtime/outbox_drainer_loop.go:49, 143, 160, 257, 263` (and some pre-existing)
- `runtime/outbox_drainer.go:285, 379, 476, 482` (pre-P3; likely moved)

### 14. `WaitGroup.Go` modernizations

Replace `wg.Add(1); go func() { defer wg.Done(); ... }()` with `wg.Go(func() { ... })`. Go 1.25+.

- `runtime/bridge.go:283` (and 1156 pre-decomposition — now in a split file)
- `runtime/outbox_drainer_loop.go:175, 192, 411` (some pre-P3)

### 15. `range over int` modernizations

Replace `for i := 0; i < n; i++` with `for i := range n`. Go 1.22+.

- `runtime/route_runner_pullpause_test.go:101, 235, 252`
- `runtime/shared_outbox_transient_recovery_test.go:142`
- `runtime/outbox_drainer_timeout_test.go:170, 278`

### 16. `t.Context()` modernizations

Replace `ctx, cancel := context.WithCancel(context.Background()); defer cancel()` with `ctx := t.Context()`. Go 1.24+.

- `runtime/shared_outbox_transient_recovery_test.go:129`
- `runtime/s13_delivery_panic_test.go:73, 162, 392, 439` (unflagged earlier — different pattern with `runDone` channel; may not apply directly).

### 17. `slices.Contains` modernization

- `bridge/supervisor.go:432` — replace linear scan with `slices.Contains`.

### 18. Nil-check redundancy

- `adapters/mqtt/transport/paho/headers_test.go:361` — `if m == nil || len(m) == 0` can be just `len(m) == 0`.

---

## Dead Code / Unused Functions

### 19. `convert.go` unused functions

- **File:** `bridge/convert.go` (or similar location — grep for these symbols).
- **Functions:** `toSessionSpec`, `toReceiverSpec`, `toSenderSpec`, `findSender`.
- **Decision needed:** are these for future config-schema work (keep with `//nolint:unused // planned`) or dead code (remove)?
- **Recommendation:** `git log -p --follow` to see when they were added and whether a planned consumer exists. If not, delete — backward compat is unaffected since they're unexported.

---

## Oversized Test Files (Below Priority, Optional)

Per CLAUDE.md, "code files" ≤ 500 LOC. Ambiguous whether tests count. These exceed:

### 20. `runtime/route_runner_test.go` — 943 LOC

- Split into themed files (e.g., `route_runner_lifecycle_test.go`, `route_runner_dispatch_test.go`, `route_runner_error_test.go`).
- Low priority — tests work, and test-file size is arguably less important than production file size.

### 21. `runtime/outbox_drainer_test.go` — 515 LOC

- Just over the limit. Could peel off the stale-token + fencing tests into a new file.

---

## Pre-existing Formatting Drift

### 22. `bridge/builder_test.go` — gofmt alignment whitespace

- Agent noticed pre-existing unrelated alignment whitespace drift. Not touched to keep refactor minimal.
- **Fix:** `gofmt -w bridge/builder_test.go` and verify no semantic changes.

---

## Reference Files for Context

When working any of the above:

- **Investigation report:** `~/.claude/plans/do-a-thorough-investigation-staged-ullman.md`
- **Credential refresh pattern:** `adapters/mqtt/transport/paho/session_credentials.go`, `bridge/credential_refresh.go`, `runtime/credentials_poll.go`
- **Scaled drain timeout:** `runtime/outbox_drainer_timeout.go` (computeBatchDeadline)
- **Processor panic/timeout:** `runtime/processor_chain.go` — reference for how resilience features get wired to `RoutePolicy`
- **Capability interface pattern:** `bridge.CredentialAware` — use this pattern to add per-transport capabilities without bloating `ports.Session`
