package dynamodboutbox

// MetricClaimScanPages is the adapter-owned counter incremented (by the number
// of DynamoDB Query pages scanned, tagged with shared.TagKeyPartition) whenever
// a single Claim's page count crosses deepBacklogPageWarn.
//
// Claim pages the WHOLE partition to guarantee oldest-first delivery (H1), so a
// sustained deep backlog — e.g. draining after an egress outage on an exclusive
// session — makes each Claim O(backlog) and draining N records cost ~N/limit
// scans (quadratic). This counter, paired with the loud WARN emitted at the same
// threshold, makes that cost observable instead of a silent quadratic drain. The
// durable fix is a per-partition created_at range key/GSI so the Query returns
// age-ordered items and the scan can stop after `limit`; see the
// claimRetentionFactor documentation in acl_store.go and ADR 0005.
//
// It is declared here (rather than in domain/shared) because it is specific to
// this DynamoDB adapter's paging strategy — mirroring the adapter-owned metric
// convention used by the native SQLite outbox store.
const MetricClaimScanPages = "DynamoDBOutboxClaimScanPages"
