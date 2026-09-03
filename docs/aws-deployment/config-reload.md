# Hot-reload and production config updates

## Hot-Reload

GoBridge watches the bridge config file for changes using a poll-based
watcher. When the file content changes, the runtime is rebuilt and swapped
in without process restart.

### Reload Sequence

```mermaid
sequenceDiagram
    participant EFS as EFS (bridge.yaml)
    participant W as Poll Watcher
    participant M as Config Manager
    participant A as App (bootstrap)
    participant RT as Runtime

    loop every poll_interval
        W->>EFS: Read file + SHA-256 hash
        alt content changed
            W->>M: Emit new BridgeConfig
            M->>M: Merge layers + validate
            M->>A: Config change event
            A->>A: resolveInputs (SSM keys)
            A->>RT: Build new runtime
            A->>RT: Start new runtime
            A->>RT: Stop old runtime
        end
    end
```

### Why Poll Mode Instead of Notify

The bootstrap library forces **poll mode** (`ModePoll`) for file watching.
The default `notify` mode uses `fsnotify` (kernel inotify/kqueue events),
which does not reliably propagate across NFS mounts. EFS is an NFS-based
file system, so writes from one Fargate task or an external writer may not
trigger inotify events on other tasks. Poll mode reads the file at a fixed
interval and compares SHA-256 hashes, which works reliably regardless of the
underlying filesystem.

### Poll Interval Tuning

| Environment | Recommended `poll_interval` | Rationale |
|-------------|----------------------------|-----------|
| Development | `"1s"` (default) | Fast feedback during local iteration. |
| Staging | `"5s"` | Balance between responsiveness and EFS read cost. |
| Production | `"5s"` to `"30s"` | Lower EFS I/O; config changes are infrequent. |

### Swap Modes

When a config change is detected, the bootstrap library must swap the old
runtime for the new one. The swap strategy is **auto-detected** based on the
transport capabilities declared by the registered factories.

**Overlap mode** (default): The new runtime is started first, then the old
runtime is stopped. This provides zero-downtime for stateless transports
like HTTP and SQS where multiple concurrent listeners are safe.

**Prepare/commit mode**: The old runtime is stopped first, then the new
runtime is built and started. This is required for transports that declare
the `CapExclusiveIdentity` capability (e.g. MQTT), where two simultaneous
connections with the same client ID would cause disconnects.

```mermaid
flowchart TD
    DETECT[Config change detected] --> CHECK{Any session transport\nhas CapExclusiveIdentity?}
    CHECK -- No --> OVERLAP[Overlap Mode]
    CHECK -- Yes --> PREPARE[Prepare/Commit Mode]

    OVERLAP --> O1[Start new runtime]
    O1 --> O2[Stop old runtime]
    O2 --> DONE[Swap complete]

    PREPARE --> P1[Stop old runtime]
    P1 --> P2[Complete + start new runtime]
    P2 --> DONE

    style OVERLAP fill:#27ae60,stroke:#333,color:#fff
    style PREPARE fill:#e67e22,stroke:#333,color:#fff
```

If the new runtime fails to start in prepare/commit mode, the bootstrap
library attempts to **recover the previous configuration** by rebuilding and
restarting the old runtime. This prevents a bad config push from leaving the
bridge in a stopped state.

---

## Config Updates in Production

Follow this workflow for safe configuration updates in production.

### Update Flow

1. **Update the YAML on EFS.** Use CI/CD, a manual mount, or the admin API
   config-transaction flow (open → patch → commit). The commit enforces
   optimistic concurrency via the `version` field (check-and-set). There is no
   `PUT /config` endpoint — see
   [HTTP API — Config transactions](../http-api-admin.md#config-transactions).

2. **Poll watcher detects the change.** Within one `poll_interval` cycle, the
   watcher reads the file, computes the SHA-256 hash, and detects the
   difference.

3. **Config is parsed and validated.** The YAML is deserialized into a
   `BridgeConfig` struct. The `validateFilesystemProfile` function checks
   topology constraints (e.g. `shared_outbox` routes are rejected under
   `filesystem_replicated` topology).

4. **SSM parameters are resolved.** The `resolveInputs` function reads
   `admin_api_key_param`, `monitor_api_key_param`, and any
   `http_receiver_api_key_params` / `http_sender_api_key_params` from SSM
   Parameter Store with decryption.

5. **New runtime is built and swapped in.** The appropriate swap mode
   (overlap or prepare/commit) is selected and the runtime is replaced.

6. **If validation or build fails:** The change is rejected, the last good
   runtime continues running, and a warning is logged:
   ```
   bootstrap: config reload rejected; keeping last good runtime  error="..."
   ```

### The Version Field

The `version` field in `BridgeConfig` is an integer counter incremented on
each config commit via the admin API. When multiple instances share the same
config file on EFS, this field provides optimistic concurrency control:

```yaml
version: 7
bridge:
  id: my-bridge
  deployment_mode: clustered
# ...
```

A config-transaction commit includes the current `version` value. The write
succeeds only if the on-disk version matches. If another instance updated the
file first, the commit fails with a conflict (`409`), and you should re-read
and retry. A `version` of `0` (or absent) means the config has never been
committed through the API.

---
