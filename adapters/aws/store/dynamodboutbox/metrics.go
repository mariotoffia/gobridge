package dynamodboutbox

// MetricClaimScanPages is the adapter-owned counter incremented (by the number
// of DynamoDB Query pages scanned, tagged with shared.TagKeyPartition) whenever
// a single Claim's page count crosses deepBacklogPageWarn ON THE SCAN FALLBACK
// PATH (a table lacking the ClaimIndex GSI).
//
// On the fallback path Claim pages the WHOLE partition to guarantee oldest-first
// delivery (H1), so a sustained deep backlog — e.g. draining after an egress
// outage on an exclusive session — makes each Claim O(backlog) and draining N
// records cost ~N/limit scans (quadratic). This counter, paired with the loud
// WARN emitted at the same threshold, makes that cost observable instead of a
// silent quadratic drain. The durable fix — now implemented — is the sparse
// age-ordered ClaimIndex GSI (PK=PK, SK=claim_sort): when present, Claim queries
// it oldest-first and STOPS after `limit`, so this scan-page counter is only
// ever emitted on an un-migrated table. See the "Claim ordering" section in
// doc.go, the claimRetentionFactor documentation in acl_store.go, and ADR 0005.
//
// It is declared here (rather than in domain/shared) because it is specific to
// this DynamoDB adapter's paging strategy — mirroring the adapter-owned metric
// convention used by the native SQLite outbox store.
const MetricClaimScanPages = "DynamoDBOutboxClaimScanPages"
