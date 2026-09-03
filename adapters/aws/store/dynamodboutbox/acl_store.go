package dynamodboutbox

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/logging"
)

const (
	// DefaultTableName is the adapter default used when no table override is configured.
	DefaultTableName          = "gobridge-outbox"
	defaultCompactionGrace    = 1 * time.Hour
	defaultStaleClaimDuration = 30 * time.Second

	// defaultResolveMaxAttempts bounds how many times Complete retries a
	// RecordIDIndex GSI lookup that returns not-found. The GSI is
	// eventually consistent, so a lookup immediately after a Claim on a
	// cold key cache can lag; a small bounded retry closes the
	// duplicate-delivery window without unbounded blocking.
	//
	// ponytail: fixed small ceiling; the key cache populated by Claim is
	// the primary (miss-free) path, so this retry is only exercised on a
	// cold cache (process restart with in-flight claimed records).
	defaultResolveMaxAttempts = 5
	defaultResolveBackoff     = 50 * time.Millisecond

	skPrefix = "OUTBOX#"

	// fenceSK is the sort key of the per-partition fence-metadata row that
	// holds the monotonic max_claim_version and the seq_counter used to
	// stamp per-partition persist order. It is deliberately outside the
	// OUTBOX# key space so it is invisible to record queries and, having no
	// GSI key attributes, to every GSI as well.
	fenceSK             = "FENCE"
	attrMaxClaimVersion = "max_claim_version"

	// attrSeqCounter is the fence-row atomic counter that allocates the
	// monotonic per-partition Seq values Persist stamps on records, so
	// Claim can order same-millisecond records by persist order (the
	// ports.OutboxStore claim-ordering contract).
	attrSeqCounter = "seq_counter"

	// attrHasExpiry marks records persisted with a non-zero ExpiresAt. It
	// is the sparse hash key of ExpiryIndex: only expiry-carrying records
	// enter the index, it is written once at Persist and removed once at a
	// terminal transition (Complete/Expire) — never rewritten on Claim —
	// so state transitions cause no index write amplification (unlike the
	// former StatusIndex, which reindexed every record on every
	// transition).
	attrHasExpiry     = "has_expiry"
	hasExpiryFlag     = "1"
	expiryIndexName   = "ExpiryIndex"
	recordIDIndexName = "RecordIDIndex"

	// attrClaimSort is the age-ordered composite sort attribute that keys the
	// ClaimIndex GSI. It encodes (created_at millis, seq) as a zero-padded
	// lexicographically-sortable string (see claimSortKey), so a Query on the
	// ClaimIndex returns a partition's claimable records OLDEST-FIRST and Claim
	// can STOP after `limit` instead of scanning the whole partition.
	// It is stamped once by Persist and removed at a
	// terminal transition (Complete/Expire) so the index is sparse — it holds
	// exactly the not-yet-terminal (pending/claimed) records — mirroring the
	// has_expiry/ExpiryIndex pattern. Claim/Release never rewrite it, so a
	// reclaimed record keeps its original age position.
	attrClaimSort  = "claim_sort"
	claimIndexName = "ClaimIndex"

	// claimIndexPageLimit caps a single ClaimIndex Query page. Claim requests
	// min(limit, claimIndexPageLimit) age-ordered items per page and stops once
	// `limit` records are claimed, so a healthy pending backlog drains in O(limit)
	// Query cost regardless of partition depth. Paging continues only when a
	// page under-fills because candidates lost a fencing race or were claimed by
	// a live owner (filtered out) — bounded by contention, not by backlog.
	claimIndexPageLimit = 1000

	// claimRetentionFactor bounds how many claimable candidates a single Claim
	// RETAINS in memory while it pages the partition to exhaustion: the oldest
	// claimRetentionFactor*limit records by (CreatedAt, Seq, EnvelopeID).
	//
	// DynamoDB Query returns items in SK order (OUTBOX#<envelope_id>#<binding_id>
	// — lexicographic by envelope ID, effectively random with respect to record
	// age), so the FIRST items a query yields are NOT the oldest. The former
	// implementation stopped after a fixed 3×limit SK-order WINDOW and sorted
	// only that window; under a backlog deeper than the window an OLD record
	// whose envelope ID sorted lexicographically late was never even
	// CONSIDERED, so it starved indefinitely and per-partition ordered delivery
	// silently broke. Claim now scans EVERY claimable record in the
	// partition (paging until LastEvaluatedKey is nil) so the retained oldest-N
	// are the TRUE oldest-N, matching the ascending-(CreatedAt, Seq)
	// claim-ordering contract memory/sqlite honour via `ORDER BY created_at,
	// seq LIMIT`. Retaining only claimRetentionFactor*limit caps client memory
	// to the same ceiling as the old window while leaving a contention buffer
	// beyond `limit` (per-record claims that lose a fencing race are skipped and
	// the next-oldest retained candidate takes their place).
	//
	// Tradeoff: exhaustive paging reads the whole claimable backlog per Claim.
	// The durable optimisation is to have the QUERY return age-ordered items so
	// the scan can stop after `limit`: add a per-partition created_at range key
	// (or created_at GSI/LSI) and shrink retention to `limit`. That index must
	// be provisioned by CreateTable and verified by the factory schema preflight
	// (Preflight) — do not require operators to hand-provision it.
	claimRetentionFactor = 3

	// deepBacklogPageWarn is the number of DynamoDB Query pages a single Claim
	// may scan before the store emits ONE loud WARN (throttled to once per
	// Claim) and bumps MetricClaimScanPages. Because Claim pages the WHOLE
	// partition to guarantee oldest-first delivery, a partition whose
	// claimable backlog spans more pages than this makes each Claim O(backlog):
	// an outage-recovery deep backlog would otherwise drain quadratically and
	// SILENTLY. Crossing the threshold is the operator's signal to provision the
	// per-partition created_at GSI (see the claimRetentionFactor note / ADR
	// 0005). A Query page is up to 1MB, so 8 pages is a genuinely deep
	// partition, not routine churn.
	deepBacklogPageWarn = 8

	// claimPreallocCap caps the INITIAL capacity of the per-Claim candidate
	// buffer. Retention is claimRetentionFactor*limit and the trim threshold is
	// 2× that, so a large-but-valid operator-set DrainMaxBatchSize (limit) would
	// otherwise eagerly allocate 6*limit and could OOM on the make() before a
	// single record is scanned. The slice still grows on demand when a partition
	// genuinely holds that many claimable records; this only bounds the eager
	// allocation.
	claimPreallocCap = 4096

	// fenceRetentionFloor is the minimum TTL horizon stamped on FENCE
	// rows: fences live for max(compactGrace, fenceRetentionFloor) past
	// the last write that touches them (Persist seq allocation and Claim
	// fence raise both refresh it). Tradeoff: a partition abandoned longer
	// than this loses its fencing high-water-mark — acceptable, because a
	// fence whose partition has had no writes for 30+ days protects
	// records that were compacted away long ago, while keeping the fence
	// immortal leaks one row per ephemeral session forever.
	fenceRetentionFloor = 30 * 24 * time.Hour
)

// The claim-conflict counter emitted by claimOne is the shared kernel's
// shared.MetricOutboxClaimConflicts, tagged with the partition
// (shared.TagKeyPartition). It distinguishes per-record claim transactions
// aborted by a DynamoDB TransactionConflict — a concurrent
// Persist/Claim/Complete touched the same item — from a record-level
// ConditionalCheckFailed (another claimer legitimately won the record, which
// is normal). Under concurrent Persist+Claim on one hot partition a rising
// value explains why a Claim returned fewer than `limit` records because of
// CONTENTION rather than an empty backlog (lag), which is otherwise silent.

// counterMeter is the minimal metrics surface this store needs: a single
// Counter method. It is declared locally (rather than importing
// ports.MetricsExporter) so this driven-adapter leaf keeps depending only on
// domain/shared per the architecture rules; ports.MetricsExporter and its
// Noop/Recording implementations satisfy it structurally, so the composition
// root can inject a real exporter through WithMetrics with no adapter glue.
type counterMeter interface {
	Counter(name string, value int64, tags ...shared.Tag)
}

// noopMeter is the default counterMeter: it discards every counter so the
// store never depends on a configured backend.
type noopMeter struct{}

func (noopMeter) Counter(string, int64, ...shared.Tag) {}

// Store implements ports.OutboxStore using DynamoDB with partitioned
// persistence, conditional writes, and TTL-based compaction. It also
// implements ports.OutboxReleaser so a live owner can return a
// transiently-failed record to pending without waiting for stale reclaim.
//
// Key design:
//
//	PK = "<partition_key>" (e.g. "SESSION#<session_id>" or "BINDING#<binding_id>")
//	SK = "OUTBOX#<envelope_id>#<binding_id>"
//
// The SK incorporates both envelope_id and binding_id so that:
//   - Fan-out records (same envelope, different bindings) have distinct keys
//   - Idempotent persist uses attribute_not_exists(SK) to skip records
//     already stored for the same envelope+binding combination across
//     redeliveries (per-record Persist idempotency, ports.OutboxStore)
//
// A RecordIDIndex GSI on the record_id attribute enables O(1) lookup
// by application-level record ID for the Complete operation.
type Store struct {
	client       dynamoAPI
	table        string
	compactGrace time.Duration
	staleClaim   time.Duration
	clk          clock.Clock
	logger       *slog.Logger

	resolveMaxAttempts int
	resolveBackoff     time.Duration

	// metrics receives store-level observability signals. It defaults to a
	// no-op meter so the store never depends on a configured backend; the
	// composition root injects a real exporter via WithMetrics.
	metrics counterMeter

	// keys maps an application record ID to its base-table (PK, SK),
	// populated on Claim so Complete can address the base table directly
	// instead of resolving through the eventually consistent RecordIDIndex
	// GSI (which can lag and report not-found, causing duplicate delivery).
	// It is a bounded LRU so claimed-but-never-completed entries (lease
	// churn) cannot grow it without limit; see keyCache (J).
	keys *keyCache

	// orderingBypassWarnOnce throttles the WARN emitted when a partition's
	// ordering keys push Claim off the ClaimIndex fast path onto the strongly
	// consistent scan. One per store: the condition is a property of the
	// deployment's traffic, not a transient fault, so repeating it per claim
	// would be noise.
	orderingBypassWarnOnce sync.Once

	// expiryCursors remembers, per partition, the ExpiryIndex page a sweep cut
	// short by its operation deadline stopped on, so the next sweep resumes
	// there instead of re-reading everything ahead of it (see expireByStatus).
	// An entry exists only while a partition has an interrupted pass in flight
	// and is deleted when that pass completes.
	expiryCursorMu sync.Mutex
	expiryCursors  map[string]map[string]ddbtypes.AttributeValue
}

type recordKey struct {
	pk string
	sk string
}

// Option configures a Store.
type Option func(*Store)

// WithTableName overrides the DynamoDB table name (default: "gobridge-outbox").
func WithTableName(name string) Option {
	return func(s *Store) { s.table = name }
}

// WithCompactionGrace sets the duration added to completed/expired timestamps
// before setting the DynamoDB TTL attribute for physical deletion.
func WithCompactionGrace(d time.Duration) Option {
	return func(s *Store) { s.compactGrace = d }
}

// WithStaleClaimDuration sets how old a claimed record must be before it can
// be reclaimed without a fencing-version bump (crash-recovery fallback).
// A value <= 0 disables wall-clock stale reclaim entirely: reclaim then
// happens only via a strictly higher fencing version (version-only), matching
// the memory and SQLite backends.
func WithStaleClaimDuration(d time.Duration) Option {
	return func(s *Store) { s.staleClaim = d }
}

// WithClock overrides the clock used for timestamps.
// Defaults to clock.System when nil or not set.
func WithClock(c clock.Clock) Option {
	return func(s *Store) {
		if c != nil {
			s.clk = c
		}
	}
}

// WithLogger sets the structured logger for trace/debug diagnostics.
func WithLogger(l *slog.Logger) Option {
	return func(s *Store) { s.logger = l }
}

// WithMetrics sets the metrics exporter the store emits store-level counters
// to (e.g. shared.MetricOutboxClaimConflicts). A nil exporter leaves the default
// no-op in place. The parameter is the minimal counterMeter surface; a
// ports.MetricsExporter satisfies it structurally.
func WithMetrics(m counterMeter) Option {
	return func(s *Store) {
		if m != nil {
			s.metrics = m
		}
	}
}

// WithCompleteResolveRetry tunes how Complete tolerates RecordIDIndex GSI
// replication lag when the Claim-populated key cache misses (e.g. after a
// process restart with in-flight claimed records). attempts is the total
// number of GSI lookups (>=1) and backoff is the pause between them. Values
// <= 0 leave the defaults unchanged.
func WithCompleteResolveRetry(attempts int, backoff time.Duration) Option {
	return func(s *Store) {
		if attempts > 0 {
			s.resolveMaxAttempts = attempts
		}
		if backoff > 0 {
			s.resolveBackoff = backoff
		}
	}
}

// NewStore creates a DynamoDB-backed OutboxStore.
//
// The *dynamodb.Client parameter is the SDK boundary input this ACL
// constructor exists to wrap; it is injected by the composition root and
// stored behind unexported fields.
//
//aclcheck:allow-export
func NewStore(client *dynamodb.Client, opts ...Option) *Store {
	s := &Store{
		client:       client,
		table:        DefaultTableName,
		compactGrace: defaultCompactionGrace,
		staleClaim:   defaultStaleClaimDuration,
		clk:          clock.System,

		resolveMaxAttempts: defaultResolveMaxAttempts,
		resolveBackoff:     defaultResolveBackoff,
		metrics:            noopMeter{},
		keys:               newKeyCache(defaultMaxKeyCache),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// CreateTable creates the DynamoDB table with the required schema and GSIs.
// It is idempotent: if the table already exists, it returns nil.
func (s *Store) CreateTable(ctx context.Context) error {
	if logging.DebugEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelDebug, "dynamodboutbox: create_table")
	}

	_, err := s.client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(s.table),
		KeySchema: []ddbtypes.KeySchemaElement{
			{AttributeName: aws.String("PK"), KeyType: ddbtypes.KeyTypeHash},
			{AttributeName: aws.String("SK"), KeyType: ddbtypes.KeyTypeRange},
		},
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: aws.String("PK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("SK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String(attrHasExpiry), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("expires_at"), AttributeType: ddbtypes.ScalarAttributeTypeN},
			{AttributeName: aws.String("record_id"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String(attrClaimSort), AttributeType: ddbtypes.ScalarAttributeTypeS},
		},
		GlobalSecondaryIndexes: []ddbtypes.GlobalSecondaryIndex{
			{
				// ExpiryIndex is sparse: only records persisted with a
				// non-zero ExpiresAt carry has_expiry, so the index holds
				// exactly the Expire-eligible candidates. It is written once
				// at Persist and deleted once at a terminal transition —
				// state transitions (Claim) never touch it. This replaces
				// the former StatusIndex (a table-wide hot partition keyed
				// on ≤4 status values, rewritten on every transition) and
				// the ClaimedByIndex (created but never queried).
				//
				// ponytail: all expiry-carrying records share one hash value
				// ("1"), bounding the index partition's write throughput; if
				// expiry-heavy workloads ever hit that ceiling, shard the
				// flag value (e.g. "1#<n>") and fan out the Expire query.
				IndexName: aws.String(expiryIndexName),
				KeySchema: []ddbtypes.KeySchemaElement{
					{AttributeName: aws.String(attrHasExpiry), KeyType: ddbtypes.KeyTypeHash},
					{AttributeName: aws.String("expires_at"), KeyType: ddbtypes.KeyTypeRange},
				},
				Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeKeysOnly},
			},
			{
				IndexName: aws.String(recordIDIndexName),
				KeySchema: []ddbtypes.KeySchemaElement{
					{AttributeName: aws.String("record_id"), KeyType: ddbtypes.KeyTypeHash},
				},
				Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeKeysOnly},
			},
			{
				// ClaimIndex is the per-partition age-ordered access path:
				// hash=PK, range=claim_sort, so a Query
				// returns a partition's claimable records OLDEST-FIRST and Claim
				// stops after `limit` instead of paging the whole partition to
				// find the oldest-N (which went O(backlog) after an outage). It
				// is sparse — only records carrying claim_sort (pending/claimed,
				// stamped at Persist and removed at Complete/Expire) are indexed,
				// so it holds exactly the not-yet-terminal working set. It
				// projects ALL so a claim query yields the full item without a
				// base-table read; the trade-off is claim_sort-scoped write
				// amplification on Claim, bounded by the in-flight backlog.
				IndexName: aws.String(claimIndexName),
				KeySchema: []ddbtypes.KeySchemaElement{
					{AttributeName: aws.String("PK"), KeyType: ddbtypes.KeyTypeHash},
					{AttributeName: aws.String(attrClaimSort), KeyType: ddbtypes.KeyTypeRange},
				},
				Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeAll},
			},
		},
		BillingMode: ddbtypes.BillingModePayPerRequest,
	})
	if err != nil {
		if isResourceInUse(err) {
			return nil
		}
		return wrapErr(err, "create outbox table failed", "table", s.table)
	}

	waiter := dynamodb.NewTableExistsWaiter(s.client)
	if err := waiter.Wait(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(s.table),
	}, 2*time.Minute); err != nil {
		return wrapErr(err, "wait for outbox table to exist failed", "table", s.table)
	}

	// Enable DynamoDB TTL on the "ttl" attribute so completed/expired records
	// are compacted physically. A ttl is stamped ONLY at a terminal transition:
	// Complete and Expire each set #ttl = now + compactGrace on the record they
	// finalize. Pending and claimed (still-deliverable) records carry no ttl, so
	// DynamoDB never physically reaps undelivered work — e.g. an on_expired=dlq
	// record that expired during an egress outage survives until the claim path
	// routes it, matching memory/sqlite which likewise never evict pending rows.
	// FENCE metadata rows carry a long TTL of max(compactGrace,
	// fenceRetentionFloor) past their last write, so abandoned (ephemeral/
	// rotating session) partitions are eventually cleaned up instead of leaking
	// one immortal row each. Best-effort: TTL is a housekeeping convenience, not
	// a correctness requirement, and is already enabled on re-runs.
	if _, err := s.client.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName: aws.String(s.table),
		TimeToLiveSpecification: &ddbtypes.TimeToLiveSpecification{
			Enabled:       aws.Bool(true),
			AttributeName: aws.String("ttl"),
		},
	}); err != nil {
		if s.logger != nil {
			s.logger.Warn("dynamodboutbox: enabling DynamoDB TTL failed; completed records not compacted",
				"table", s.table, "error", err.Error())
		}
	}
	return nil
}

// Persist writes outbox records with per-record idempotency
// (ports.OutboxStore Persist contract): each record is a conditional
// PutItem keyed on attribute_not_exists(SK), so records whose
// (partition, envelope, binding) identity already exists are skipped
// while new records in the same batch are persisted. It returns
// shared.ErrDuplicateRecord ONLY when every record in the batch already
// existed. Per-record writes (instead of one all-or-nothing transaction)
// are exactly what the idempotency contract makes safe: a batch that
// fails midway is simply re-persisted, and already-written legs become
// no-ops. This also removes the former 100-item transaction ceiling.
//
// Before writing, Persist allocates each record a monotonic per-partition
// Seq from the FENCE row's atomic seq_counter, so Claim can order
// same-millisecond records by persist order.
func (s *Store) Persist(ctx context.Context, records []*persistence.OutboxRecord) error {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodboutbox: persist", "count", len(records))
	}

	if len(records) == 0 {
		return nil
	}

	now := s.clk.Now()

	seqs, err := s.allocateSeqs(ctx, records, now)
	if err != nil {
		return err
	}

	duplicates := 0
	for i := range records {
		item, err := marshalRecord(records[i], now)
		if err != nil {
			return err
		}
		item["seq"] = &ddbtypes.AttributeValueMemberN{Value: u64(seqs[i])}
		// Stamp the age-ordered ClaimIndex key now that both created_at (from
		// marshalRecord) and the allocated seq are known, so Claim can query
		// the partition oldest-first and stop after `limit`.
		item[attrClaimSort] = &ddbtypes.AttributeValueMemberS{
			Value: claimSortKey(numAttrI64(item, "created_at"), seqs[i]),
		}

		_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:           aws.String(s.table),
			Item:                item,
			ConditionExpression: aws.String("attribute_not_exists(SK)"),
		})
		if err != nil {
			if isConditionFailed(err) {
				// attribute_not_exists(SK) failed: a row already occupies this
				// sort key. Verify it is the SAME logical record before treating
				// it as an idempotent duplicate. sortKey is INJECTIVE, so this
				// store can only ever produce that conflict for the SAME
				// (envelope_id, binding_id) — a genuine redelivery. The readback
				// is therefore not about ordinary duplicates: it is the guard
				// against a sort key occupied by a DIFFERENT record, which can
				// only come from a writer that is not this store. Counting that
				// as a duplicate would ack and DROP a distinct message, so it is
				// surfaced as a transient conflict and RETRIED, never dropped.
				same, verr := s.conflictIsSameRecord(ctx, item, records[i])
				if verr != nil {
					return verr
				}
				if same {
					duplicates++
					continue
				}
				return shared.ErrUnavailable.
					WithMessage("outbox persist: sort key occupied by a DIFFERENT record; "+
						"a foreign writer shares this table — retrying rather than dropping the message").
					With("envelopeID", records[i].EnvelopeID()).
					With("bindingID", records[i].BindingID())
			}
			return wrapErr(err, "outbox persist failed",
				"envelopeID", records[i].EnvelopeID(), "bindingID", records[i].BindingID())
		}
	}

	if duplicates == len(records) {
		return shared.ErrDuplicateRecord.
			WithMessage("all records in batch already persisted").
			With("recordCount", len(records))
	}
	return nil
}

// conflictIsSameRecord resolves an attribute_not_exists(SK) PutItem conflict:
// it strongly-consistently reads the row already occupying this sort key and
// reports whether that row is the SAME logical record (identical envelope_id
// AND binding_id) as the one being persisted — a genuine idempotent duplicate —
// versus a DIFFERENT record whose key collides (a pre-upgrade raw-key migration
// alias; see Persist). ConsistentRead guarantees this read observes the very
// row the failed condition saw. If the row has vanished (claimed, completed and
// TTL-compacted between the failed condition and this read) it is reported NOT
// the same, so the caller surfaces a transient conflict and the record is
// retried into the now-free slot rather than dropped.
func (s *Store) conflictIsSameRecord(
	ctx context.Context,
	item map[string]ddbtypes.AttributeValue,
	rec *persistence.OutboxRecord,
) (bool, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]ddbtypes.AttributeValue{
			"PK": item["PK"],
			"SK": item["SK"],
		},
		ConsistentRead:       aws.Bool(true),
		ProjectionExpression: aws.String("envelope_id, binding_id"),
	})
	if err != nil {
		return false, wrapErr(err, "outbox persist: resolve sort-key conflict",
			"envelopeID", rec.EnvelopeID(), "bindingID", rec.BindingID())
	}
	if out.Item == nil {
		return false, nil
	}
	return strAttr(out.Item, "envelope_id") == rec.EnvelopeID() &&
		strAttr(out.Item, "binding_id") == rec.BindingID(), nil
}

// allocateSeqs reserves one monotonic per-partition sequence number per
// record by atomically incrementing the partition FENCE row's seq_counter
// (one UpdateItem per distinct partition in the batch). The same write
// refreshes the FENCE row's TTL. Sequences consumed by records that turn
// out to be duplicates leave gaps — the contract requires monotonicity,
// not contiguity.
func (s *Store) allocateSeqs(ctx context.Context, records []*persistence.OutboxRecord, now time.Time) ([]uint64, error) {
	counts := make(map[string]int)
	for _, r := range records {
		counts[partitionKey(r)]++
	}

	next := make(map[string]uint64, len(counts))
	for pk, n := range counts {
		newTop, err := s.addSeqCounter(ctx, pk, n, now)
		if err != nil {
			return nil, err
		}
		if newTop < uint64(n) {
			// Defensive: a backend that did not return the counter would
			// otherwise underflow. Seq 0 sorts first, preserving safety.
			newTop = uint64(n)
		}
		next[pk] = newTop - uint64(n)
	}

	seqs := make([]uint64, len(records))
	for i, r := range records {
		pk := partitionKey(r)
		next[pk]++
		seqs[i] = next[pk]
	}
	return seqs, nil
}

// addSeqCounter atomically advances the partition's seq_counter by n and
// returns the new counter value. The FENCE row is created on first use and
// its TTL horizon refreshed on every allocation.
func (s *Store) addSeqCounter(ctx context.Context, partitionKey string, n int, now time.Time) (uint64, error) {
	out, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]ddbtypes.AttributeValue{
			"PK": &ddbtypes.AttributeValueMemberS{Value: partitionKey},
			"SK": &ddbtypes.AttributeValueMemberS{Value: fenceSK},
		},
		UpdateExpression: aws.String("SET #ttl = :ttl ADD #sc :n"),
		ExpressionAttributeNames: map[string]string{
			"#sc":  attrSeqCounter,
			"#ttl": "ttl",
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":n":   &ddbtypes.AttributeValueMemberN{Value: i64(int64(n))},
			":ttl": &ddbtypes.AttributeValueMemberN{Value: i64(s.fenceTTLEpoch(now))},
		},
		ReturnValues: ddbtypes.ReturnValueUpdatedNew,
	})
	if err != nil {
		return 0, wrapErr(err, "outbox seq allocation failed", "partitionKey", partitionKey)
	}
	return numAttrU64(out.Attributes, attrSeqCounter), nil
}

// fenceTTLEpoch returns the epoch-seconds TTL stamped on FENCE rows:
// now + max(compactGrace, fenceRetentionFloor). See fenceRetentionFloor
// for the abandonment tradeoff.
func (s *Store) fenceTTLEpoch(now time.Time) int64 {
	horizon := s.compactGrace
	if horizon < fenceRetentionFloor {
		horizon = fenceRetentionFloor
	}
	return now.Add(horizon).Unix()
}

// Claim finds pending or stale-claimed records for the given partition and
// claims them for the given owner with the provided fencing token.
// Uses strongly consistent reads with pagination to handle the DynamoDB
// Limit+Filter interaction (Limit caps evaluated items, not filtered results).
//
// Per the ports.OutboxStore contract, Claim never filters by replay count —
// poison detection is the drainer's decision, and a store-side filter would
// strand high-replay records (unclaimable forever, then silently TTL-reaped
// with no DLQ entry).
//
// Each per-record claim executes as a TransactWriteItems that pairs the
// record update with a ConditionCheck on the partition FENCE row
// (max_claim_version <= token.Version). This closes the TOCTOU race where
// owner A (v5) passes the fence read, owner B (v6) raises the fence and
// starts claiming, and A keeps claiming other pending records below the
// high-water-mark — split-brain duplicate delivery. With the transactional
// check, A's next claim fails the fence condition and surfaces
// shared.ErrStaleFencingToken.
func (s *Store) Claim(ctx context.Context, partitionKey string, token persistence.LeaseToken, limit int) ([]*persistence.OutboxRecord, error) {
	// Fencing guard: the DynamoDB claim path bypasses the OutboxRecord
	// aggregate and drives raw conditional writes, so it must reject an invalid
	// (zero-value) fencing token itself — BEFORE the fence read/advance and any
	// per-record TransactWriteItems. A LeaseToken with an empty Owner or zero
	// Version can never name a real lease (LeaseStore issues versions from 1),
	// so it must never claim work. See the ports.OutboxStore fencing contract
	// and persistence.LeaseToken.Valid.
	if !token.Valid() {
		return nil, shared.ErrStaleFencingToken.
			WithMessage("claim rejected: invalid (zero-value) fencing token").
			With("owner", token.Owner).
			With("version", token.Version)
	}

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodboutbox: claim", "partition_key", partitionKey, "limit", limit)
	}

	now := s.clk.Now()

	// staleMs is the wall-clock threshold (Unix ms) below which a claimed_at
	// marks a stranded claim as reclaimable. staleClaim <= 0 disables
	// wall-clock reclaim: the sentinel 0 can never exceed a real claimed_at,
	// so reclaim is version-only (matching memory/sqlite semantics).
	staleMs := int64(0)
	if s.staleClaim > 0 {
		staleMs = now.Add(-s.staleClaim).UnixMilli()
	}

	// Version-monotonic fence: reject a token whose version is older than the
	// highest claim_version the partition has already observed, so a preempted
	// owner cannot win a freshly pending row (matches memory/sqlite and the
	// ports.OutboxStore contract).
	maxVersion, err := s.maxClaimVersion(ctx, partitionKey)
	if err != nil {
		return nil, err
	}
	if token.Version < maxVersion {
		return nil, shared.ErrStaleFencingToken.
			WithMessage("claim rejected: token version is stale").
			With("givenVersion", token.Version).
			With("latestVersion", maxVersion)
	}

	// Raise the per-partition fence to this (accepted) token version so any
	// later claim with a lower version is rejected in O(1), even after the
	// records this owner claims are completed and compacted away. Raise-only
	// via a conditional write, so concurrent higher versions are preserved.
	if err := s.raiseFence(ctx, partitionKey, token.Version, now); err != nil {
		return nil, err
	}

	// limit <= 0 is a fencing no-op (ports.OutboxStore contract): the fence
	// high-water-mark above has been advanced; no records are claimed.
	if limit <= 0 {
		return nil, nil
	}

	// A claimed record is reclaimable when its claim_version is strictly
	// older than the incoming token (version-monotonic preemption, matching
	// the port contract and memory/sqlite) OR when its claim has gone stale
	// past the wall-clock threshold (crash-recovery fallback). Key-condition
	// values (:pk, and the scan fallback's :prefix) are added per path.
	filterExpr := "(#st = :pending) OR (#st = :claimed AND (claim_version < :ver OR claimed_at < :stale))"
	exprNames := map[string]string{"#st": "status"}
	filterValues := map[string]ddbtypes.AttributeValue{
		":pending": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxPending)},
		":claimed": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxClaimed)},
		":ver":     &ddbtypes.AttributeValueMemberN{Value: u64(token.Version)},
		":stale":   &ddbtypes.AttributeValueMemberN{Value: i64(staleMs)},
	}

	// Fast path: query the age-ordered ClaimIndex GSI and STOP once `limit`
	// records are claimed, so a healthy pending backlog drains in O(limit) Query
	// cost regardless of how deep the partition is. The index is REQUIRED — it
	// is provisioned by CreateTable and verified by Preflight — so a
	// query failure here is a real fault, not a reason to degrade. The one
	// legitimate hand-off is an ordering-keyed partition, which no
	// eventually-consistent index can serve correctly.
	claimed, handled, err := s.claimByIndex(ctx, partitionKey, token, limit, now, staleMs, filterExpr, exprNames, filterValues)
	if handled {
		return claimed, err
	}
	return s.claimByScan(ctx, partitionKey, token, limit, now, staleMs, exprNames)
}

// claimByIndex is the O(limit) claim path: it queries the age-ordered
// ClaimIndex GSI (range=claim_sort, ScanIndexForward=true → oldest-first) and
// collects candidates as it pages, STOPPING as soon as it holds enough instead
// of scanning the whole partition. Because the GSI yields items in ascending
// (CreatedAt, Seq), the claimed slice is already in claim-order without a
// client-side sort.
//
// handled is false (with a nil error) for exactly one condition: a candidate
// carries an ORDERING KEY, so the claim belongs on the strongly consistent
// path. Every other failure is returned as an error — the ClaimIndex is
// required, provisioned by CreateTable and verified by Preflight,
// so a missing or under-projected index is a provisioning fault to surface, not
// a mode to silently degrade into.
//
// The ordering-key condition is what makes DynamoDB honour per-key order. A GSI
// cannot be read strongly consistent, and index propagation is per item and
// unordered: records A (older) and B (younger) sharing a key can both be
// written successfully and the index can surface B before A. Claiming in index
// order would then send B first, with zero failures anywhere — an ordering
// violation no test against DynamoDB Local can reproduce, because its GSIs are
// synchronously consistent. So the moment this path sees ANY ordering key it
// abandons the index, having claimed nothing, and the caller re-runs the claim
// through claimByScan, which reads the base table with ConsistentRead: true and
// therefore sees every sibling. Keyless partitions keep the O(limit) fast path;
// keyed ones pay the exhaustive-scan cost for correct order. This is why
// candidates are COLLECTED first and claimed second — a claim issued while
// paging could not be taken back once a keyed record appeared on a later page.
func (s *Store) claimByIndex(
	ctx context.Context,
	partitionKey string,
	token persistence.LeaseToken,
	limit int,
	now time.Time,
	staleMs int64,
	filterExpr string,
	exprNames map[string]string,
	filterValues map[string]ddbtypes.AttributeValue,
) (claimed []*persistence.OutboxRecord, handled bool, err error) {
	exprValues := make(map[string]ddbtypes.AttributeValue, len(filterValues)+1)
	for k, v := range filterValues {
		exprValues[k] = v
	}
	exprValues[":pk"] = &ddbtypes.AttributeValueMemberS{Value: partitionKey}

	pageLimit := int32(claimIndexPageLimit)
	if limit < claimIndexPageLimit {
		pageLimit = int32(limit)
	}

	claimed = make([]*persistence.OutboxRecord, 0, limit)
	var startKey map[string]ddbtypes.AttributeValue
	for len(claimed) < limit {
		if err := ctx.Err(); err != nil {
			// Records claimed on earlier pages are durably this owner's; a
			// deadline must truncate the batch, never discard them.
			out, _, truncErr := s.truncatedClaim(claimed, err, partitionKey)
			return out, true, truncErr
		}
		input := &dynamodb.QueryInput{
			TableName:                 aws.String(s.table),
			IndexName:                 aws.String(claimIndexName),
			KeyConditionExpression:    aws.String("PK = :pk"),
			FilterExpression:          aws.String(filterExpr),
			ExpressionAttributeNames:  exprNames,
			ExpressionAttributeValues: exprValues,
			// Ascending claim_sort == oldest-first (the claim-ordering contract).
			ScanIndexForward: aws.Bool(true),
			Limit:            aws.Int32(pageLimit),
		}
		if startKey != nil {
			input.ExclusiveStartKey = startKey
		}

		out, err := s.client.Query(ctx, input)
		if err != nil {
			queryErr := wrapErr(err, "outbox claim index query failed", "partitionKey", partitionKey)
			res, _, truncErr := s.truncatedClaim(claimed, queryErr, partitionKey)
			return res, true, truncErr
		}

		// Vet the page BEFORE claiming any of it: a claim already issued cannot
		// be taken back once a keyed record appears. Items are only inspected up
		// to what this page contributes to `limit`; anything past that is
		// YOUNGER (the index is age-ordered) and so can never be the older
		// sibling of a record claimed here.
		//
		// ponytail: candidates that lose a per-record fencing race are not
		// backfilled from the rest of the page — the batch just comes back
		// short, which the port contract allows, and the next cycle starts at
		// the head again. Losing that race needs a second claimer on a
		// lease-fenced partition, which the design forbids; the scan path keeps
		// its 3x retention buffer because it pages anyway. If contention ever
		// shows up here (OutboxClaimConflicts on the index path), refill from
		// the remaining page items instead of paging on.
		want := limit - len(claimed)
		candidates := make([]claimCandidate, 0, min(want, len(out.Items)))
		for _, item := range out.Items {
			if len(candidates) >= want {
				break
			}
			c := newClaimCandidate(item, true)
			if c.orderingKey != "" {
				s.logClaimIndexOrderingBypass(partitionKey)
				if len(claimed) == 0 {
					// Nothing claimed yet: hand the whole claim to the strongly
					// consistent scan path, which can see every sibling.
					return nil, false, nil
				}
				// Keyless records from earlier pages are already claimed and
				// carry no ordering constraint. Stop here with a legally short
				// batch; the next cycle finds this keyed record at the head and
				// takes the scan path.
				return claimed, true, nil
			}
			candidates = append(candidates, c)
		}

		// Every candidate here is keyless, so the head-of-line rule cannot apply.
		var truncated bool
		claimed, truncated, err = s.claimCandidates(ctx, claimed, candidates, token, now, staleMs, limit, partitionKey)
		if err != nil {
			return nil, true, err
		}
		if truncated {
			// A per-record write failed transiently after earlier records were
			// claimed. Paging on would issue more reads against a store that is
			// already failing, and could re-trip the truncation counter; the
			// short batch is legal, so stop here.
			return claimed, true, nil
		}

		if out.LastEvaluatedKey == nil {
			break // index exhausted for this partition
		}
		startKey = out.LastEvaluatedKey
	}
	return claimed, true, nil
}

// logClaimIndexOrderingBypass emits one throttled WARN the first time a
// partition's claim leaves the index fast path because its records carry
// ordering keys. It is not a fault — it is the documented cost of strict
// per-key order on DynamoDB (ADR 0005) — but an operator seeing
// DynamoDBOutboxClaimScanPages rise on a table that HAS the ClaimIndex needs to
// know why.
func (s *Store) logClaimIndexOrderingBypass(partitionKey string) {
	s.orderingBypassWarnOnce.Do(func() {
		if s.logger != nil {
			s.logger.Warn(
				"dynamodboutbox: partition carries ordering keys, so Claim reads the base table with "+
					"ConsistentRead instead of the eventually-consistent ClaimIndex GSI — a lagging index "+
					"can surface a younger same-key record first. Claim cost is O(partition backlog) for "+
					"this partition; see ADR 0005",
				"partition_key", partitionKey,
				"index", claimIndexName,
			)
		}
	})
}

// claimByScan is the strongly consistent claim path, used for every partition
// that carries ordering keys: only a base-table ConsistentRead can prove a
// record has no older non-terminal sibling, and no global secondary index can be
// read that way.
//
// It scans the WHOLE partition (paging until LastEvaluatedKey is nil),
// retaining only the oldest claimRetentionFactor*limit candidates by
// (CreatedAt, Seq, EnvelopeID), then claims the oldest N. DynamoDB Query
// returns items in SK order (lexicographic by envelope ID, unrelated to record
// age), so the FIRST items encountered are NOT the oldest; exhaustive paging
// proves the true oldest-N, at O(backlog) cost — the very cost the ClaimIndex
// fast path exists to avoid. Bounded retention keeps client memory flat; a deep
// backlog is surfaced via deepBacklogPageWarn.
//
// The scan reads every NON-TERMINAL record, not only the claimable ones, so a
// record left Claimed by a dead owner is visible as a BLOCKER for its ordering
// key. Filtering to claimable records alone would hide exactly the head that
// must stall its younger siblings.
func (s *Store) claimByScan(
	ctx context.Context,
	partitionKey string,
	token persistence.LeaseToken,
	limit int,
	now time.Time,
	staleMs int64,
	exprNames map[string]string,
) ([]*persistence.OutboxRecord, error) {
	// Non-terminal, not merely claimable: see the blocker rule above. Claimable
	// is decided client-side by isClaimableItem, which mirrors the predicate the
	// index path pushes to the server, so the version/stale placeholders the
	// index filter binds have no counterpart here.
	const nonTerminalExpr = "(#st = :pending) OR (#st = :claimed)"
	exprValues := map[string]ddbtypes.AttributeValue{
		":pk":      &ddbtypes.AttributeValueMemberS{Value: partitionKey},
		":prefix":  &ddbtypes.AttributeValueMemberS{Value: skPrefix},
		":pending": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxPending)},
		":claimed": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxClaimed)},
	}

	retain := limit * claimRetentionFactor
	if retain < limit {
		// Overflow guard for a pathologically large limit: never retain fewer
		// than `limit` candidates.
		retain = limit
	}

	// candidates is trimmed back to the oldest `retain` whenever it grows past
	// trimAt, so a partition whose claimable backlog is far deeper than `retain`
	// cannot grow client memory without bound while the scan still CONSIDERS
	// every record.
	trimAt := retain * 2
	if trimAt < retain { // overflow guard
		trimAt = retain
	}
	// ponytail: cap the EAGER allocation at claimPreallocCap so a huge-but-valid
	// operator-set limit (DrainMaxBatchSize) cannot OOM on this make() before the
	// trim guard ever runs. The slice still grows on demand if the partition
	// genuinely holds more than claimPreallocCap claimable records.
	initialCap := trimAt
	if initialCap > claimPreallocCap {
		initialCap = claimPreallocCap
	}
	candidates := make([]claimCandidate, 0, initialCap)
	// blocked holds, per ordering key, the oldest non-terminal record this claim
	// CANNOT take — the stranded head its younger siblings must wait behind. It
	// retains positions only, never items, so a deep fully-keyed backlog cannot
	// pin one payload per key for the length of the claim.
	blocked := make(blockedHeads)
	trim := func() {
		if len(candidates) <= retain {
			return
		}
		sortOldestFirst(candidates)
		candidates = candidates[:retain]
	}

	var startKey map[string]ddbtypes.AttributeValue
	// Claim pages the whole partition, so a deep backlog makes this loop
	// O(backlog). Count pages/records scanned and, once a single Claim crosses
	// deepBacklogPageWarn, emit ONE loud WARN (throttled via deepBacklogWarned)
	// plus a partition-tagged counter so an outage-recovery quadratic drain on
	// this fallback path is observable, not silent. The durable fix is the
	// ClaimIndex GSI (provision it to move onto the O(limit) fast path).
	pages := 0
	recordsScanned := 0
	deepBacklogWarned := false
	for {
		input := &dynamodb.QueryInput{
			TableName:                 aws.String(s.table),
			KeyConditionExpression:    aws.String("PK = :pk AND begins_with(SK, :prefix)"),
			FilterExpression:          aws.String(nonTerminalExpr),
			ExpressionAttributeNames:  exprNames,
			ExpressionAttributeValues: exprValues,
			ConsistentRead:            aws.Bool(true),
		}
		if startKey != nil {
			input.ExclusiveStartKey = startKey
		}

		queryOut, err := s.client.Query(ctx, input)
		if err != nil {
			return nil, wrapErr(err, "outbox claim query failed", "partitionKey", partitionKey)
		}
		pages++
		recordsScanned += len(queryOut.Items)

		for _, item := range queryOut.Items {
			c := newClaimCandidate(item, isClaimableItem(item, token.Version, staleMs))
			if c.claimable {
				candidates = append(candidates, c)
				continue
			}
			// Non-terminal but not claimable: a head stranded by a dead owner.
			// It is not work this claim can take, but it must stall its own
			// key's younger siblings.
			blocked.observe(c)
		}
		if len(candidates) > trimAt {
			trim()
		}

		if !deepBacklogWarned && pages > deepBacklogPageWarn {
			deepBacklogWarned = true
			if s.logger != nil {
				s.logger.Warn(
					"dynamodboutbox: deep outbox backlog; Claim is reading the whole partition "+
						"(O(backlog)) because this partition carries ordering keys, which no "+
						"eventually-consistent index can serve — see doc.go / ADR 0005 / docs/runbooks",
					"partition_key", partitionKey,
					"pages", pages,
					"records_scanned", recordsScanned,
				)
			}
		}

		if queryOut.LastEvaluatedKey == nil {
			break
		}
		startKey = queryOut.LastEvaluatedKey

		// A pathologically deep backlog can page many times; honour
		// cancellation between pages so a Claim never outlives its context.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	// A deep-backlog Claim scanned an unusual number of pages: surface the count
	// (tagged by partition) so the O(backlog) per-Claim cost is measurable, not
	// just log-visible. s.metrics defaults to a no-op meter, so no nil guard.
	if pages > deepBacklogPageWarn {
		s.metrics.Counter(MetricClaimScanPages, int64(pages),
			shared.Tag{Key: shared.TagKeyPartition, Value: partitionKey})
	}

	// Oldest-first: ascending (CreatedAt, Seq) with envelopeID as the legacy
	// tiebreak for two records that somehow share a position, matching
	// QueryPending and the
	// ports.OutboxStore claim-ordering contract. The retained set already holds
	// the globally oldest `retain`; this final sort orders them for claiming.
	sortOldestFirst(candidates)
	if len(candidates) > retain {
		candidates = candidates[:retain]
	}
	// Ordering-key head-of-line: drop anything sitting behind a stranded head.
	// Applied AFTER the sort and BEFORE the claim loop truncates at `limit`, so
	// a group whose head falls outside the batch takes its tail with it.
	candidates = dropBlockedByStrandedHead(candidates, blocked)

	claimed, _, err := s.claimCandidates(ctx, nil, candidates, token, now, staleMs, limit, partitionKey)
	return claimed, err
}

// claimOne transitions a single candidate record to claimed inside a
// TransactWriteItems that also condition-checks the partition FENCE row, so
// a concurrently raised fence (higher-version owner took the partition)
// aborts the claim instead of silently split-braining.
//
// Returns (nil, nil) when only the record-level condition failed — another
// claimer won this record; the caller skips it. Returns
// shared.ErrStaleFencingToken when the fence check failed.
//
// TransactWriteItems cannot return the updated item, so the post-claim
// record is synthesized from the queried item plus the exact values the
// update wrote — the transaction's conditions guarantee they took effect.
func (s *Store) claimOne(
	ctx context.Context,
	item map[string]ddbtypes.AttributeValue,
	pk, sk string,
	token persistence.LeaseToken,
	now time.Time,
	staleMs int64,
) (*persistence.OutboxRecord, error) {
	_, err := s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []ddbtypes.TransactWriteItem{
			{
				ConditionCheck: &ddbtypes.ConditionCheck{
					TableName: aws.String(s.table),
					Key: map[string]ddbtypes.AttributeValue{
						"PK": &ddbtypes.AttributeValueMemberS{Value: pk},
						"SK": &ddbtypes.AttributeValueMemberS{Value: fenceSK},
					},
					ConditionExpression:      aws.String("attribute_not_exists(#mcv) OR #mcv <= :ver"),
					ExpressionAttributeNames: map[string]string{"#mcv": attrMaxClaimVersion},
					ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
						":ver": &ddbtypes.AttributeValueMemberN{Value: u64(token.Version)},
					},
				},
			},
			{
				Update: &ddbtypes.Update{
					TableName: aws.String(s.table),
					Key: map[string]ddbtypes.AttributeValue{
						"PK": &ddbtypes.AttributeValueMemberS{Value: pk},
						"SK": &ddbtypes.AttributeValueMemberS{Value: sk},
					},
					UpdateExpression: aws.String(
						"SET #st = :claimed, claimed_by = :owner, claim_version = :ver, " +
							"claimed_at = :now, " +
							"first_attempted_at = if_not_exists(first_attempted_at, :fa), " +
							"replay_count = if_not_exists(replay_count, :zero) + :one"),
					ConditionExpression: aws.String(
						"(#st = :pending) OR (#st = :claimed AND (claim_version < :ver OR claimed_at < :stale))"),
					ExpressionAttributeNames: map[string]string{"#st": "status"},
					ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
						":claimed": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxClaimed)},
						":pending": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxPending)},
						":owner":   &ddbtypes.AttributeValueMemberS{Value: token.Owner},
						":ver":     &ddbtypes.AttributeValueMemberN{Value: u64(token.Version)},
						":now":     &ddbtypes.AttributeValueMemberN{Value: i64(now.UnixMilli())},
						// :fa stamps the first-attempt clock only when absent
						// (if_not_exists), so a reclaim never moves it.
						":fa":    &ddbtypes.AttributeValueMemberN{Value: i64(now.UnixMilli())},
						":stale": &ddbtypes.AttributeValueMemberN{Value: i64(staleMs)},
						":zero":  &ddbtypes.AttributeValueMemberN{Value: "0"},
						":one":   &ddbtypes.AttributeValueMemberN{Value: "1"},
					},
				},
			},
		},
	})
	if err != nil {
		if reasons, canceled := transactCancellationCodes(err); canceled {
			// Defensive: a TransactionCanceledException with NO per-item reason
			// codes is not a recognisable lost race. It must NOT fall through to
			// the benign (nil, nil) skip below — an empty/malformed cancellation
			// would then silently DROP the record. Surface it as a (transient)
			// fault via wrapErr so the drainer retries instead (c13 review LOW).
			if len(reasons) == 0 {
				return nil, wrapErr(err, "outbox claim transaction canceled with no reasons",
					"partitionKey", pk, "ownerID", token.Owner)
			}
			// Item 0 is the fence check: its failure means a higher-version
			// owner raised the fence between our read and this claim — the
			// exact TOCTOU split-brain the transaction exists to prevent.
			//
			// PRECEDENCE: the fence ConditionCheck is item 0 and is evaluated
			// FIRST, so it is deliberately handled before nonContentionCancellation
			// below. A failed fence check means THIS owner has lost the partition
			// to a higher version — it must stop claiming and re-fence — so even a
			// co-occurring throttle on the record update (item 1) is moot: there
			// is nothing to back off and retry under this now-stale token.
			// Surfacing ErrStaleFencingToken (not a throttle backoff) is therefore
			// the correct, higher-priority outcome.
			if len(reasons) > 0 && reasons[0] == ccReasonCondCheckFailed {
				return nil, shared.ErrStaleFencingToken.
					WithMessage("claim aborted: partition fence advanced past token version").
					With("givenVersion", token.Version).
					With("partitionKey", pk)
			}
			// A cancellation reason OUTSIDE the benign contention set
			// (ProvisionedThroughputExceeded / ThrottlingError / ValidationError
			// / ...) is NOT a lost race. Returning (nil, nil) here — as the code
			// once did for EVERY non-fence reason — silently SKIPPED the record
			// with no backoff signal, so a throttled partition self-throttled
			// harder and validation faults hid forever.
			// Classify through wrapErr so throttling surfaces as retryable
			// shared.ErrThrottled (the drainer backs off) and permanent faults
			// surface honestly, instead of being dropped.
			if code, faulted := nonContentionCancellation(reasons); faulted {
				return nil, wrapErr(err, "outbox claim transaction canceled",
					"partitionKey", pk, "ownerID", token.Owner, "cancellationReason", code)
			}
			// Record-level condition failure or a transient transaction
			// conflict: another claimer won this record. Skip it. A
			// TransactionConflict (a concurrent Persist/Claim/Complete touched
			// the same item) is counted so a Claim that returns fewer than
			// `limit` records because of CONTENTION is distinguishable from an
			// empty backlog (lag); a record-level ConditionalCheckFailed is a
			// normal lost race and is not counted.
			if hasCode(reasons, ccReasonTxnConflict) {
				s.metrics.Counter(shared.MetricOutboxClaimConflicts, 1,
					shared.Tag{Key: shared.TagKeyPartition, Value: pk})
			}
			return nil, nil
		}
		return nil, wrapErr(err, "outbox claim update failed", "partitionKey", pk, "ownerID", token.Owner)
	}

	// Synthesize the post-claim state on a copy of the queried item.
	updated := make(map[string]ddbtypes.AttributeValue, len(item)+5)
	for k, v := range item {
		updated[k] = v
	}
	updated["status"] = &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxClaimed)}
	updated["claimed_by"] = &ddbtypes.AttributeValueMemberS{Value: token.Owner}
	updated["claim_version"] = &ddbtypes.AttributeValueMemberN{Value: u64(token.Version)}
	updated["claimed_at"] = &ddbtypes.AttributeValueMemberN{Value: i64(now.UnixMilli())}
	updated["replay_count"] = &ddbtypes.AttributeValueMemberN{Value: i64(numAttrI64(item, "replay_count") + 1)}
	// Mirror the if_not_exists(first_attempted_at, :fa) write: stamp it only
	// when the queried item had no first_attempted_at, so a reclaim of an
	// already-attempted record keeps the original instant.
	if _, ok := updated["first_attempted_at"]; !ok {
		updated["first_attempted_at"] = &ddbtypes.AttributeValueMemberN{Value: i64(now.UnixMilli())}
	}

	return unmarshalRecord(updated)
}

// Complete marks the given records as completed after successful target delivery.
// The caller's fencing token must match the claim_version on each record.
func (s *Store) Complete(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error {
	// Fencing guard: the completion fence is owner+version+status enforced
	// via raw conditional UpdateItems that bypass the OutboxRecord aggregate, so
	// reject an invalid (zero-value) token here — BEFORE resolving any record
	// key or issuing an UpdateItem. An empty Owner or zero Version can never
	// match a real claim's non-empty owner and non-zero version, so it must
	// never complete work. See the ports.OutboxStore fencing contract and
	// persistence.LeaseToken.Valid.
	if !token.Valid() {
		return shared.ErrStaleFencingToken.
			WithMessage("complete rejected: invalid (zero-value) fencing token").
			With("owner", token.Owner).
			With("version", token.Version)
	}

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodboutbox: complete", "count", len(recordIDs))
	}

	if len(recordIDs) == 0 {
		return nil
	}

	now := s.clk.Now()
	ttlEpoch := now.Add(s.compactGrace).Unix()

	for _, id := range recordIDs {
		pk, sk, err := s.resolveRecordKeys(ctx, id)
		if err != nil {
			return err
		}
		if pk == "" {
			// The Claim-populated key cache missed (e.g. a cold cache after a
			// process restart) and the eventually consistent RecordIDIndex GSI
			// still had not converged after the bounded resolve retry. The
			// record was just claimed, so it exists in the base table — this is
			// GSI replication lag, not a genuine absence. A permanent
			// ErrNotFound here would tell the caller to give up completing and
			// let the record be re-DELIVERED on the next stale/version reclaim;
			// a transient timeout instead keeps it claimed so the caller
			// retries Complete once the GSI catches up, closing the
			// duplicate-delivery window (at-least-once still holds). See.
			return shared.ErrTimeout.
				WithMessage("outbox record key resolution timed out: RecordIDIndex GSI lag").
				With("recordID", id)
		}

		_, err = s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName: aws.String(s.table),
			Key: map[string]ddbtypes.AttributeValue{
				"PK": &ddbtypes.AttributeValueMemberS{Value: pk},
				"SK": &ddbtypes.AttributeValueMemberS{Value: sk},
			},
			// REMOVE has_expiry drops the terminal record out of the sparse
			// ExpiryIndex so Expire never re-scans it; REMOVE claim_sort drops
			// it out of the sparse ClaimIndex so Claim never re-considers it.
			UpdateExpression: aws.String(
				"SET #st = :completed, completed_at = :now, #ttl = :ttl REMOVE " + attrHasExpiry + ", " + attrClaimSort),
			ConditionExpression: aws.String(
				"#st = :claimed AND claimed_by = :owner AND claim_version = :ver"),
			ExpressionAttributeNames: map[string]string{
				"#st":  "status",
				"#ttl": "ttl",
			},
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":completed": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxCompleted)},
				":claimed":   &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxClaimed)},
				":now":       &ddbtypes.AttributeValueMemberN{Value: i64(now.UnixMilli())},
				":ttl":       &ddbtypes.AttributeValueMemberN{Value: i64(ttlEpoch)},
				":owner":     &ddbtypes.AttributeValueMemberS{Value: token.Owner},
				":ver":       &ddbtypes.AttributeValueMemberN{Value: u64(token.Version)},
			},
		})
		if err != nil {
			if isConditionFailed(err) {
				return shared.ErrStaleFencingToken.
					WithMessage("claim version mismatch on complete").
					With("recordID", id).
					With("givenVersion", token.Version)
			}
			return wrapErr(err, "outbox complete update failed", "recordID", id, "ownerID", token.Owner)
		}
		// Record is terminal; drop its cached keys.
		s.evictKey(id)
	}

	return nil
}

// Release implements ports.OutboxReleaser: it returns transiently-failed
// claimed records to pending immediately so a still-alive owner can retry
// them on the next drain, without a fencing-version bump or a stale-claim
// timeout. Fencing is owner+version+status, identical to Complete; on any
// mismatch it returns shared.ErrStaleFencingToken.
//
// Releases are applied per record in order and stop at the first mismatch
// (documented in the ports.OutboxReleaser contract); the live drainer
// always passes exactly one record ID. claim_version is retained on the
// released record — the record is pending again, so it is claimable by any
// token at or above the partition fence.
func (s *Store) Release(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error {
	// Fencing guard: Release fencing is owner+version+status, identical to
	// Complete, applied via raw conditional UpdateItems that bypass the
	// OutboxRecord aggregate, so reject an invalid (zero-value) token here —
	// BEFORE resolving any record key or issuing an UpdateItem. An empty Owner
	// or zero Version can never match a real claim, so it must never release
	// work. See the ports.OutboxReleaser fencing contract and
	// persistence.LeaseToken.Valid.
	if !token.Valid() {
		return shared.ErrStaleFencingToken.
			WithMessage("release rejected: invalid (zero-value) fencing token").
			With("owner", token.Owner).
			With("version", token.Version)
	}

	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodboutbox: release", "count", len(recordIDs))
	}

	for _, id := range recordIDs {
		pk, sk, err := s.resolveRecordKeys(ctx, id)
		if err != nil {
			return err
		}
		if pk == "" {
			// Same GSI-lag reasoning as Complete: the record was just
			// claimed, so absence is index lag, not a genuine miss. Surface
			// a transient error so the caller can retry the release; the
			// record stays claimed and remains stale-reclaimable.
			return shared.ErrTimeout.
				WithMessage("outbox record key resolution timed out: RecordIDIndex GSI lag").
				With("recordID", id)
		}

		_, err = s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName: aws.String(s.table),
			Key: map[string]ddbtypes.AttributeValue{
				"PK": &ddbtypes.AttributeValueMemberS{Value: pk},
				"SK": &ddbtypes.AttributeValueMemberS{Value: sk},
			},
			// claimed_by/claimed_at are removed to match the unclaimed item
			// shape Persist writes; has_expiry is untouched (Claim never
			// removed it), so a released expiry-carrying record remains
			// visible to Expire.
			UpdateExpression: aws.String("SET #st = :pending REMOVE claimed_by, claimed_at"),
			ConditionExpression: aws.String(
				"#st = :claimed AND claimed_by = :owner AND claim_version = :ver"),
			ExpressionAttributeNames: map[string]string{"#st": "status"},
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":pending": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxPending)},
				":claimed": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxClaimed)},
				":owner":   &ddbtypes.AttributeValueMemberS{Value: token.Owner},
				":ver":     &ddbtypes.AttributeValueMemberN{Value: u64(token.Version)},
			},
		})
		if err != nil {
			if isConditionFailed(err) {
				return shared.ErrStaleFencingToken.
					WithMessage("claim mismatch on release").
					With("recordID", id).
					With("givenVersion", token.Version)
			}
			return wrapErr(err, "outbox release update failed", "recordID", id, "ownerID", token.Owner)
		}
	}

	return nil
}

// QueryPending returns up to `limit` pending records for the given partition
// key, ordered oldest-first — ascending (CreatedAt, Seq) — WITHIN the returned
// set, using strongly consistent reads and paginating past DynamoDB's
// Limit+Filter interaction.
//
// SELECTION semantics (count/preview, NOT oldest-N): DynamoDB Query returns
// items in SK order (OUTBOX#<envelope_id>#… — lexicographic by envelope ID,
// unrelated to record age). When a partition holds MORE than `limit` pending
// records this method collects the first `limit` items DynamoDB yields (SK
// order) and then sorts THAT subset oldest-first, so the returned set is NOT
// guaranteed to be the globally oldest-N — only internally age-ordered. This
// is deliberate: the sole runtime caller counts pending records against
// MaxOutboxDepth (runtime/route/dispatch.go), where only the COUNT matters and
// the identity of the sampled records is immaterial, so QueryPending avoids the
// read amplification of Claim's exhaustive oldest-N scan.
//
// Claim, by contrast, DOES select the oldest-N (it pages the whole partition to
// exhaustion and retains the globally oldest candidates before claiming) because
// per-partition send ordering depends on it. Callers that need oldest-N
// SELECTION must use Claim, not QueryPending.
//
// ponytail: if a future caller needs oldest-N selection here, give QueryPending
// the same exhaustive-scan treatment as Claim, or add a created_at range key /
// per-partition GSI so the Query returns age-ordered items directly (which
// would let both methods drop the client-side sort).
func (s *Store) QueryPending(ctx context.Context, partitionKey string, limit int) ([]*persistence.OutboxRecord, error) {
	if logging.TraceEnabled(s.logger) {
		s.logger.Log(ctx, logging.LevelTrace, "dynamodboutbox: query_pending", "partition_key", partitionKey, "limit", limit)
	}

	var records []*persistence.OutboxRecord
	var startKey map[string]ddbtypes.AttributeValue

	for len(records) < limit {
		input := &dynamodb.QueryInput{
			TableName:              aws.String(s.table),
			KeyConditionExpression: aws.String("PK = :pk AND begins_with(SK, :prefix)"),
			FilterExpression:       aws.String("#st = :pending"),
			ExpressionAttributeNames: map[string]string{
				"#st": "status",
			},
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":pk":      &ddbtypes.AttributeValueMemberS{Value: partitionKey},
				":prefix":  &ddbtypes.AttributeValueMemberS{Value: skPrefix},
				":pending": &ddbtypes.AttributeValueMemberS{Value: string(persistence.OutboxPending)},
			},
			ConsistentRead: aws.Bool(true),
		}
		if startKey != nil {
			input.ExclusiveStartKey = startKey
		}

		out, err := s.client.Query(ctx, input)
		if err != nil {
			return nil, wrapErr(err, "outbox query pending failed", "partitionKey", partitionKey)
		}

		for _, item := range out.Items {
			rec, err := unmarshalRecord(item)
			if err != nil {
				return nil, err
			}
			records = append(records, rec)
		}

		if out.LastEvaluatedKey == nil {
			break
		}
		startKey = out.LastEvaluatedKey
	}

	// Per-partition persist order — ascending (CreatedAt, Seq) with
	// envelopeID as the legacy fallback, matching the Claim ordering
	// contract.
	sort.Slice(records, func(i, j int) bool {
		ci, cj := records[i].CreatedAt(), records[j].CreatedAt()
		if !ci.Equal(cj) {
			return ci.Before(cj)
		}
		if records[i].Seq() != records[j].Seq() {
			return records[i].Seq() < records[j].Seq()
		}
		return records[i].EnvelopeID() < records[j].EnvelopeID()
	})

	if len(records) > limit {
		records = records[:limit]
	}

	return records, nil
}

// maxClaimVersion returns the monotonic fence for the partition: the
// highest claim_version any owner has ever used to claim in this partition
// (0 if none). It reads a single per-partition fence-metadata row (O(1),
// strongly consistent) rather than scanning the whole partition.
//
// Cold partition: a partition only Persist has ever touched has a fence row
// whose max_claim_version attribute is absent, and a partition whose fence row
// has aged past its TTL has none at all (see fenceRetentionFloor). In both cases
// the fence is seeded once from a bounded scan of the existing records'
// claim_version and persisted, so the high-water-mark cannot reset under records
// that still carry a claim — and the O(N) scan happens at most once per fence
// lifetime.
func (s *Store) maxClaimVersion(ctx context.Context, partitionKey string) (uint64, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table),
		Key: map[string]ddbtypes.AttributeValue{
			"PK": &ddbtypes.AttributeValueMemberS{Value: partitionKey},
			"SK": &ddbtypes.AttributeValueMemberS{Value: fenceSK},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return 0, wrapErr(err, "outbox fence read failed", "partitionKey", partitionKey)
	}
	// Check the ATTRIBUTE, not row presence: a fence row created solely by
	// Persist's seq allocation carries seq_counter but no max_claim_version
	// yet and must still fall through to the cold seed.
	if _, ok := out.Item[attrMaxClaimVersion]; ok {
		return numAttrU64(out.Item, attrMaxClaimVersion), nil
	}

	// Cold partition: seed the fence from existing records once.
	seed, err := s.maxClaimVersionByScan(ctx, partitionKey)
	if err != nil {
		return 0, err
	}
	if seed > 0 {
		if err := s.raiseFence(ctx, partitionKey, seed, s.clk.Now()); err != nil {
			return 0, err
		}
	}
	return seed, nil
}

// raiseFence raises the partition fence row's max_claim_version to version
// using a raise-only conditional write, so concurrent claims with a higher
// version are never clobbered by a lower one. The same write refreshes the
// fence row's TTL horizon (see fenceRetentionFloor).
func (s *Store) raiseFence(ctx context.Context, partitionKey string, version uint64, now time.Time) error {
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]ddbtypes.AttributeValue{
			"PK": &ddbtypes.AttributeValueMemberS{Value: partitionKey},
			"SK": &ddbtypes.AttributeValueMemberS{Value: fenceSK},
		},
		UpdateExpression:    aws.String("SET #mcv = :ver, #ttl = :ttl"),
		ConditionExpression: aws.String("attribute_not_exists(#mcv) OR #mcv < :ver"),
		ExpressionAttributeNames: map[string]string{
			"#mcv": attrMaxClaimVersion,
			"#ttl": "ttl",
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":ver": &ddbtypes.AttributeValueMemberN{Value: u64(version)},
			":ttl": &ddbtypes.AttributeValueMemberN{Value: i64(s.fenceTTLEpoch(now))},
		},
	})
	if err != nil {
		// Condition failure means a concurrent claim already advanced the
		// fence to >= version — the monotonic invariant still holds.
		if isConditionFailed(err) {
			return nil
		}
		return wrapErr(err, "outbox fence raise failed", "partitionKey", partitionKey)
	}
	return nil
}

// maxClaimVersionByScan computes the fence seed for a cold partition by
// scanning existing record rows (bounded to one full partition read; used
// once per partition to migrate to the fence row).
func (s *Store) maxClaimVersionByScan(ctx context.Context, partitionKey string) (uint64, error) {
	var maxVersion uint64
	var startKey map[string]ddbtypes.AttributeValue

	for {
		out, err := s.client.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(s.table),
			KeyConditionExpression: aws.String("PK = :pk AND begins_with(SK, :prefix)"),
			ExpressionAttributeNames: map[string]string{
				"#cv": "claim_version",
			},
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":pk":     &ddbtypes.AttributeValueMemberS{Value: partitionKey},
				":prefix": &ddbtypes.AttributeValueMemberS{Value: skPrefix},
			},
			ProjectionExpression: aws.String("#cv"),
			ConsistentRead:       aws.Bool(true),
			ExclusiveStartKey:    startKey,
		})
		if err != nil {
			return 0, wrapErr(err, "outbox max claim version query failed", "partitionKey", partitionKey)
		}

		for _, item := range out.Items {
			if v := numAttrU64(item, "claim_version"); v > maxVersion {
				maxVersion = v
			}
		}

		if out.LastEvaluatedKey == nil {
			break
		}
		startKey = out.LastEvaluatedKey
	}

	return maxVersion, nil
}

// resolveRecordKeys returns the base-table (PK, SK) for an application record
// ID. It first consults the key cache populated by Claim (the miss-free path),
// then falls back to the eventually consistent RecordIDIndex GSI with a
// bounded retry to absorb GSI replication lag — closing the not-found →
// duplicate-delivery window a single GSI read would leave open.
func (s *Store) resolveRecordKeys(ctx context.Context, recordID string) (string, string, error) {
	if k, ok := s.lookupKey(recordID); ok {
		return k.pk, k.sk, nil
	}

	var lastPK, lastSK string
	for attempt := 0; ; attempt++ {
		pk, sk, err := s.resolveRecordKeysFromGSI(ctx, recordID)
		if err != nil {
			return "", "", err
		}
		if pk != "" {
			return pk, sk, nil
		}
		lastPK, lastSK = pk, sk

		if attempt >= s.resolveMaxAttempts-1 {
			break
		}
		if err := s.sleep(ctx, s.resolveBackoff); err != nil {
			return "", "", err
		}
	}
	return lastPK, lastSK, nil
}

func (s *Store) resolveRecordKeysFromGSI(ctx context.Context, recordID string) (string, string, error) {
	out, err := s.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		IndexName:              aws.String(recordIDIndexName),
		KeyConditionExpression: aws.String("record_id = :id"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":id": &ddbtypes.AttributeValueMemberS{Value: recordID},
		},
		Limit: aws.Int32(1),
	})
	if err != nil {
		return "", "", wrapErr(err, "outbox resolve record keys failed", "recordID", recordID)
	}
	if len(out.Items) == 0 {
		return "", "", nil
	}
	return strAttr(out.Items[0], "PK"), strAttr(out.Items[0], "SK"), nil
}

// sleep blocks for d honoring context cancellation, using the injected
// clock so tests remain deterministic.
func (s *Store) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := s.clk.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C():
		return nil
	}
}

func (s *Store) cacheKey(recordID, pk, sk string) {
	s.keys.put(recordID, recordKey{pk: pk, sk: sk})
}

func (s *Store) lookupKey(recordID string) (recordKey, bool) {
	return s.keys.get(recordID)
}

func (s *Store) evictKey(recordID string) {
	s.keys.remove(recordID)
}
