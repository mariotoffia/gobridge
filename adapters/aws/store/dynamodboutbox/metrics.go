package dynamodboutbox

// MetricClaimScanPages is the adapter-owned counter incremented (by the number
// of DynamoDB Query pages scanned, tagged with shared.TagKeyPartition) whenever
// a single Claim's page count crosses deepBacklogPageWarn ON THE STRONGLY
// CONSISTENT SCAN PATH (every ordering-keyed partition).
//
// On that path Claim pages the WHOLE partition to guarantee oldest-first
// delivery, so a sustained deep backlog — e.g. draining after an egress outage
// on an exclusive session — makes each Claim O(backlog) and draining N records
// cost ~N/limit scans (quadratic). This counter, paired with the loud WARN
// emitted at the same threshold, makes that cost observable instead of a silent
// quadratic drain.
//
// Keyless partitions never reach it: they are served by the sparse age-ordered
// ClaimIndex GSI (PK=PK, SK=claim_sort), which returns records oldest-first so
// Claim STOPS after `limit`. A rising counter therefore means ORDERING KEYS —
// no eventually-consistent index can prove a keyed record has no older unseen
// sibling, so keyed partitions read the base table consistently. See the "Claim
// ordering" section in doc.go, the claimRetentionFactor documentation in
// acl_store.go, and ADR 0005.
//
// It is declared here (rather than in domain/shared) because it is specific to
// this DynamoDB adapter's paging strategy — mirroring the adapter-owned metric
// convention used by the native SQLite outbox store.
const MetricClaimScanPages = "DynamoDBOutboxClaimScanPages"

// MetricClaimTruncated is the adapter-owned counter incremented (tagged with
// shared.TagKeyPartition) whenever a Claim ends early because a per-record
// transaction failed transiently — a throttle, a deadline, a network fault —
// AFTER earlier records were already durably claimed.
//
// Such a batch is legally SHORT: the records already claimed are returned so the
// drainer sends them, rather than being discarded with the error and left
// Claimed, hidden from CountPending, until the wall-clock stale window (each
// recovery cycle charging them a replay attempt). This counter is what makes
// that truncation visible; a rising value means the claim loop is being cut
// short, which usually reads as sustained throttling or a claim budget too small
// for the batch size. It is declared here because it is specific to this
// backend's per-record claim transactions — SQLite claims in one transaction and
// the in-memory store under one mutex, so neither can truncate.
const MetricClaimTruncated = "DynamoDBOutboxClaimTruncated"
