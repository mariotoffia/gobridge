package dynamodblease

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/shared"
)

// Preflight validates that the configured DynamoDB table's key schema matches
// what the LEASE role needs and ENFORCES that DynamoDB TTL is DISABLED on it. It
// is a build-time safeguard against a misprovisioned or copy-pasted table name
// and against a fatal TTL misconfiguration.
//
// The lease row IS the monotonic fencing counter of record. Two dangers:
//   - Wrong key schema (e.g. an outbox/DLQ table with extra keys or a different
//     partition key) corrupts lease storage.
//   - DynamoDB TTL ENABLED on the lease table lets a reaper delete lease/fence
//     rows, resetting their version to 1 while the outbox HWM sits at v≫1 —
//     every subsequent claim then fails ErrStaleFencingToken and the partition
//     stalls (a split-brain window). TTL-enabled is legitimate on OTHER tables
//     but is a correctness hazard here, so on the lease table an OBSERVED enabled
//     TTL is FATAL — not a mere WARN — unless WithTTLPreflightAdvisory is set.
//
// Semantics:
//   - Table missing (ResourceNotFound): NON-fatal, returns nil (build-then-
//     EnsureTable flows stay valid; a missing table fails loudly at first use).
//   - Table present with a mismatched schema: FATAL, returns a
//     shared.ErrInvalidConfig error naming the table, expected, and actual.
//   - Table present with TTL ENABLED/ENABLING: FATAL, returns a
//     shared.ErrInvalidConfig error naming the table and ttl status — unless
//     WithTTLPreflightAdvisory downgrades it to a loud WARN (dev/emulator).
//   - DescribeTimeToLive itself fails (missing permission / throttle / emulator
//     gap): FATAL fail-closed (it proves nothing about the TTL state) — unless
//     WithTTLPreflightAdvisory downgrades it to a loud WARN.
//
// Expected schema (see EnsureTable / doc.go):
//
//	Primary key : PK (S, HASH)   — no range key, no GSIs, TTL DISABLED
func (s *Store) Preflight(ctx context.Context) error {
	out, err := s.client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(s.tableName),
	})
	if err != nil {
		if isResourceNotFound(err) {
			return nil
		}
		return wrapErr(err, "lease preflight: describe table failed", "table", s.tableName)
	}

	if err := validateTableSchema(s.tableName, "dynamodblease", out.Table,
		[]expectedKey{
			{name: attrPK, keyType: ddbtypes.KeyTypeHash, attrType: ddbtypes.ScalarAttributeTypeS},
		},
		nil,
	); err != nil {
		return err
	}

	// The lease table exists: ENFORCE the TTL-DISABLED invariant. A reaper
	// deleting fencing rows breaks token monotonicity, so an observed enabled
	// TTL (or an unverifiable TTL state) is fatal by default.
	return s.checkLeaseTableTTL(ctx)
}

// checkLeaseTableTTL enforces the TTL-DISABLED invariant on the lease table at
// build time. The lease row IS the monotonic
// fencing counter of record: an ENABLED (or ENABLING) DynamoDB TTL lets a reaper
// delete a fence row and reset its version to 1 while the outbox high-water mark
// sits at v≫1, opening a split-brain window in which every subsequent claim
// fails ErrStaleFencingToken. An OBSERVED enabled TTL is therefore a FATAL
// configuration error (shared.ErrInvalidConfig), not a WARN.
//
// A DescribeTimeToLive call that itself FAILS (missing dynamodb:DescribeTimeToLive,
// a throttle, or an emulator that does not implement it) proves NOTHING about the
// table's TTL state and must NOT be swallowed as success: it is surfaced
// FAIL-CLOSED. Crucially it is returned as shared.ErrInvalidConfig (with the
// classified transport cause wrapped for diagnostics), NOT as a bare transient
// sentinel: the DynamoDBStoreFactory's fatal decision rests on
// errors.Is(err, ErrInvalidConfig) — the BridgeError Code identity — which it
// checks BEFORE the advisory branch (acl_factory.go), whereas any OTHER error
// funnels through its "could-not-verify" path where WithSchemaPreflightAdvisory()
// would downgrade it. Returning the ErrInvalidConfig Code ensures ONLY
// WithTTLPreflightAdvisory (the TTL-specific opt-out, handled below) can relax
// this TTL check — the SCHEMA advisory must not silently disable it. (Any
// .With(...) context added to the error is diagnostic only and plays no part in
// that decision.)
//
// The single escape hatch is WithTTLPreflightAdvisory(), an EXPLICIT dev/emulator
// opt-out that downgrades BOTH an observed enabled TTL and a DescribeTimeToLive
// error to a loud WARN and returns nil. Production leaves it unset.
func (s *Store) checkLeaseTableTTL(ctx context.Context) error {
	out, err := s.client.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{
		TableName: aws.String(s.tableName),
	})
	if err != nil {
		if s.ttlPreflightAdvisory {
			s.warnTTLPreflightUnverified(err)
			return nil
		}
		// Fail closed AND factory-always-fatal. The factory's fatal decision rests
		// SOLELY on errors.Is(err, shared.ErrInvalidConfig) — the BridgeError Code
		// identity, which survives the WithMessage/With/Wrap builder chain — so
		// returning ErrInvalidConfig is precisely what blocks boot regardless of the
		// factory's SCHEMA advisory. The classified transport cause
		// (ErrThrottled/ErrNotAuthorized/…) is wrapped so errors.Is still finds it
		// for diagnostics. Only WithTTLPreflightAdvisory (above) relaxes this.
		//
		// The .With("ttl_preflight","unverified") below is DIAGNOSTIC context only
		// (a structured-log / telemetry breadcrumb) — it plays NO part in the fatal
		// decision, which is the Code identity above.
		return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"dynamodblease: could not verify DynamoDB TTL state on lease table %q "+
				"(DescribeTimeToLive failed); refusing to start because a TTL-reaped "+
				"fence row is a split-brain hazard. Grant dynamodb:DescribeTimeToLive, "+
				"or set WithTTLPreflightAdvisory for a dev/emulator that cannot "+
				"DescribeTimeToLive.", s.tableName)).
			With("table", s.tableName).
			With("ttl_preflight", "unverified").
			Wrap(mapError(err))
	}
	if out == nil || out.TimeToLiveDescription == nil {
		return nil
	}
	switch out.TimeToLiveDescription.TimeToLiveStatus {
	case ddbtypes.TimeToLiveStatusEnabled, ddbtypes.TimeToLiveStatusEnabling:
		status := string(out.TimeToLiveDescription.TimeToLiveStatus)
		if s.ttlPreflightAdvisory {
			s.warnTTLEnabled(status)
			return nil
		}
		return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
			"dynamodblease: DynamoDB TTL is %s on lease table %q; the lease row is the "+
				"fencing counter of record and a TTL reaper deleting it resets the fencing "+
				"version and opens a split-brain window. Disable table TTL (set "+
				"WithTTLPreflightAdvisory only for a dev/emulator).",
			status, s.tableName)).
			With("table", s.tableName).
			With("ttl_status", status)
	}
	return nil
}

// warnTTLEnabled emits the loud advisory-mode WARN for an observed enabled TTL
// on the lease table (WithTTLPreflightAdvisory). Nil-logger safe.
func (s *Store) warnTTLEnabled(status string) {
	if s.logger == nil {
		return
	}
	s.logger.Warn(
		"dynamodblease: DynamoDB TTL is ENABLED on the lease table under "+
			"WithTTLPreflightAdvisory; it is the fencing counter of record and TTL MUST "+
			"be DISABLED or fencing-token monotonicity will break. Disable table TTL "+
			"immediately (the advisory override is for dev/emulator only).",
		"table", s.tableName,
		"ttl_status", status,
	)
}

// warnTTLPreflightUnverified emits the loud advisory-mode WARN when
// DescribeTimeToLive could not verify the lease-table TTL state
// (WithTTLPreflightAdvisory). Nil-logger safe.
func (s *Store) warnTTLPreflightUnverified(err error) {
	if s.logger == nil {
		return
	}
	s.logger.Warn(
		"dynamodblease: lease-table TTL preflight skipped (DescribeTimeToLive failed) "+
			"under WithTTLPreflightAdvisory; production must grant dynamodb:DescribeTimeToLive "+
			"so preflight can enforce the TTL-DISABLED invariant on the fencing table.",
		"table", s.tableName,
		"error", err.Error(),
	)
}

// isResourceNotFound reports whether err is a DynamoDB ResourceNotFoundException
// (the table does not exist yet), which Preflight treats as non-fatal.
func isResourceNotFound(err error) bool {
	var rnf *ddbtypes.ResourceNotFoundException
	return errors.As(err, &rnf)
}

// expectedKey describes one required key attribute of a table or index.
type expectedKey struct {
	name     string
	keyType  ddbtypes.KeyType
	attrType ddbtypes.ScalarAttributeType
}

// expectedIndex describes a required global secondary index.
type expectedIndex struct {
	name string
	keys []expectedKey
}

// validateTableSchema verifies the described table's primary key and required
// GSIs match the role's expectation, returning a precise shared.ErrInvalidConfig
// on the first mismatch. Extra (unexpected) GSIs are tolerated.
func validateTableSchema(
	tableName, pkg string,
	table *ddbtypes.TableDescription,
	primary []expectedKey,
	gsis []expectedIndex,
) error {
	if table == nil {
		return schemaMismatch(pkg, tableName, "DescribeTable returned an empty table description")
	}
	defs := attrTypeIndex(table.AttributeDefinitions)

	if err := checkKeySchema(pkg, tableName, "primary key", table.KeySchema, defs, primary); err != nil {
		return err
	}

	for _, want := range gsis {
		got := findGSI(table.GlobalSecondaryIndexes, want.name)
		if got == nil {
			return schemaMismatch(pkg, tableName, fmt.Sprintf(
				"required global secondary index %q is missing (expected key schema %s)",
				want.name, renderExpected(want.keys)))
		}
		if err := checkKeySchema(pkg, tableName, fmt.Sprintf("GSI %q", want.name), got.KeySchema, defs, want.keys); err != nil {
			return err
		}
	}
	return nil
}

func checkKeySchema(
	pkg, tableName, what string,
	schema []ddbtypes.KeySchemaElement,
	defs map[string]ddbtypes.ScalarAttributeType,
	want []expectedKey,
) error {
	actual := renderActual(schema, defs)
	byRole := make(map[ddbtypes.KeyType]ddbtypes.KeySchemaElement, len(schema))
	for _, e := range schema {
		byRole[e.KeyType] = e
	}

	for _, k := range want {
		got, ok := byRole[k.keyType]
		if !ok {
			return schemaMismatch(pkg, tableName, fmt.Sprintf(
				"%s is missing its %s key; expected %s, actual %s",
				what, strings.ToLower(string(k.keyType)), renderExpected(want), actual))
		}
		if aws.ToString(got.AttributeName) != k.name {
			return schemaMismatch(pkg, tableName, fmt.Sprintf(
				"%s %s key is %q, expected %q; expected %s, actual %s",
				what, strings.ToLower(string(k.keyType)), aws.ToString(got.AttributeName), k.name,
				renderExpected(want), actual))
		}
		if at, ok := defs[k.name]; ok && at != k.attrType {
			return schemaMismatch(pkg, tableName, fmt.Sprintf(
				"%s key %q has attribute type %q, expected %q; expected %s, actual %s",
				what, k.name, string(at), string(k.attrType), renderExpected(want), actual))
		}
	}

	// A range key present where none is expected is a schema mismatch too (e.g.
	// a lease table pointed at a composite-key outbox table): reject it so the
	// error is symmetric.
	if elem, hasRange := byRole[ddbtypes.KeyTypeRange]; hasRange && !wantsRole(want, ddbtypes.KeyTypeRange) {
		return schemaMismatch(pkg, tableName, fmt.Sprintf(
			"%s has an unexpected range key %q; expected %s, actual %s",
			what, aws.ToString(elem.AttributeName), renderExpected(want), actual))
	}
	return nil
}

func attrTypeIndex(defs []ddbtypes.AttributeDefinition) map[string]ddbtypes.ScalarAttributeType {
	m := make(map[string]ddbtypes.ScalarAttributeType, len(defs))
	for _, d := range defs {
		m[aws.ToString(d.AttributeName)] = d.AttributeType
	}
	return m
}

func findGSI(gsis []ddbtypes.GlobalSecondaryIndexDescription, name string) *ddbtypes.GlobalSecondaryIndexDescription {
	for i := range gsis {
		if aws.ToString(gsis[i].IndexName) == name {
			return &gsis[i]
		}
	}
	return nil
}

func wantsRole(want []expectedKey, role ddbtypes.KeyType) bool {
	for _, k := range want {
		if k.keyType == role {
			return true
		}
	}
	return false
}

func renderExpected(keys []expectedKey) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s(%s,%s)", k.name, k.attrType, k.keyType)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func renderActual(schema []ddbtypes.KeySchemaElement, defs map[string]ddbtypes.ScalarAttributeType) string {
	sorted := append([]ddbtypes.KeySchemaElement(nil), schema...)
	sort.SliceStable(sorted, func(i, j int) bool { return keyRoleOrder(sorted[i].KeyType) < keyRoleOrder(sorted[j].KeyType) })
	parts := make([]string, 0, len(sorted))
	for _, e := range sorted {
		name := aws.ToString(e.AttributeName)
		parts = append(parts, fmt.Sprintf("%s(%s,%s)", name, defs[name], e.KeyType))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func keyRoleOrder(k ddbtypes.KeyType) int {
	if k == ddbtypes.KeyTypeHash {
		return 0
	}
	return 1
}

func schemaMismatch(pkg, tableName, detail string) error {
	return shared.ErrInvalidConfig.WithMessage(fmt.Sprintf(
		"%s: table %q schema mismatch: %s", pkg, tableName, detail))
}
