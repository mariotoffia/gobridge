# Runbook: Image Upgrade / Rollback and SQLite Durability

**Applies to:** container deployments of GoBridge (ECS, Kubernetes, plain Docker).
**Audience:** operators rolling image versions and anyone running a SQLite store
in a container.
**Risk:** high for SQLite on ephemeral storage — a scale-in or redeploy can drop
undelivered messages silently. Read the durability section before you scale.

## Image version rollout and rollback

### Roll out

1. Build and push the new image, pinned by digest (see below).
2. Update the task/pod image reference and deploy one instance (or a canary
   worker) first.
3. Gate on readiness before shifting traffic — do **not** trust `/health` alone,
   which stays green before sessions connect
   ([deployment-guide.md#health-endpoints](../deployment-guide.md#health-endpoints)):

   ```bash
   curl -s "http://<host>:8081/api/v1/monitor/ready?level=subscribed"   # 200 → every subscription acked
   ```

4. Roll the rest of the fleet once the canary is healthy.

### Roll back

Redeploy the previous image digest. Because the process exits non-zero on an
unrecoverable startup or runtime fault, a task that fails to start on the new
image is restarted by the orchestrator rather than left wedged
([deployment-guide.md#exit-codes](../deployment-guide.md#exit-codes)). If a bad
**config** rode along with the image, revert it with the transactions API — see
[Config Rollback](config-rollback.md).

### Pin images by digest

For reproducible builds, pin images by digest (`name@sha256:...`) rather than a
floating tag such as `:latest`. This applies to the base and runtime images in
the `Dockerfile` (`FROM ...@sha256:...`) and to the GoBridge image referenced in
ECS task definitions or Kubernetes pod specs. A moving tag makes a rebuild
non-reproducible and can pull an unexpected image on the next deploy. Resolve a
tag to its digest once, then reference the digest:

```bash
docker buildx imagetools inspect ghcr.io/mariotoffia/gobridge:v0.2.0 \
  --format '{{json .Manifest.Digest}}'
```

No image tags are published yet: the release workflow pushes the image **by
digest only** (never `ghcr.io/...:vX.Y.Z`) after the first `cmd/gobridge/vX.Y.Z`
command release is cut, recording the verified digest in
`gobridge-image-digest.txt`. The `v0.1.0` / `v0.2.0` tags above are illustrative
placeholders — until the first release, take the authoritative digest straight
from that release asset rather than resolving a tag ([RELEASE.md](../../RELEASE.md)).

## SQLite store durability

### The hard requirement

**A containerized SQLite store must live on a durable volume.** The SQLite
outbox, lease, DLQ, and managed-subscription stores keep delivery-critical records on disk. If that disk is
the container's ephemeral filesystem, stopping or replacing the task destroys any
records it still holds — silent message loss with no error.

The AWS CDK profile guards against this: the Phase-1 validator rejects any store
path that is not under the EFS mount root, returning `ErrStorePathOutsideMount`.
**Operators wiring their own Kubernetes or Docker deployment get no such guard.**
Put every SQLite store path on a persistent volume (a `PersistentVolumeClaim`, a
bind-mounted host path, or a network filesystem), never on the container's
writable layer or an `emptyDir`.

### Safe scale-in / replace

Scaling in a task whose SQLite outbox still holds undelivered records on
ephemeral storage loses them. Before you remove or replace an instance:

1. Stop new work reaching it (drain the receiver, or route to `direct_hold`).
2. Let the drainer empty the outbox. Confirm `OutboxDepth` reaches 0
   ([monitoring.md#key-metrics](../aws-deployment/monitoring.md#key-metrics)).
   Set `MaxOutboxDepth` so `OutboxDepth` reports the true backlog rather than a
   saturating batch-size floor — otherwise a deep backlog is invisible
   ([monitoring.md#key-metrics](../aws-deployment/monitoring.md#key-metrics)).
3. Only then stop the task.

`SQLiteStoreUnhealthy` (`entity=outbox`) is a fatal storage fault — disk full,
corruption, read-only, or not-a-database. Alert on it directly; it means free
disk or restore the file, and records stay durable until you do
([monitoring.md#key-metrics](../aws-deployment/monitoring.md#key-metrics)).

### Backup and restore

The SQLite store is a single file per store (plus the WAL/SHM sidecars) on the
durable volume. To back it up consistently:

1. Quiesce writers to that store — drain the outbox to 0 (above), or stop the
   task — so the file is not mid-write.
2. Copy the store file together with any `-wal` and `-shm` sidecars, or use
   `sqlite3 <file> ".backup <dest>"` for an online snapshot.
3. Store the copy off the task's own volume.

To restore, place the file back on the durable volume at the configured store
path before the task starts, then start the task. A restored outbox replays its
undelivered records on first drain.

## Related runbooks

- [Config rollback](config-rollback.md)
- [Poison message / DLQ growth](poison-message-dlq-growth.md)
- [Persistent MQTT managed-filter migration](mqtt-managed-subscription-migration.md)
