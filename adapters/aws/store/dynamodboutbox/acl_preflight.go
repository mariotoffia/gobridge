package dynamodboutbox

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

// Preflight validates that the configured DynamoDB table's key schema and
// required global secondary indexes match what the OUTBOX role needs. It is a
// build-time safeguard against a misprovisioned or copy-pasted table name.
//
// The failure it guards against: an outbox pointed at a PK-only table (e.g. a
// lease-table name copy-pasted — the config shapes are identical) would accept
// the FIRST record per partition and then classify every subsequent record a
// duplicate (attribute_not_exists(SK) fails against the existing PK-only item),
// which the dispatcher acks and drops — a silent message shredder (H3).
//
// Semantics:
//   - Table missing (ResourceNotFound): NON-fatal, returns nil. Build-then-
//     create flows (CreateTable after construction) stay valid, and a genuinely
//     missing table fails loudly at the first operation — not silent loss.
//   - Table present with a mismatched schema: FATAL. Returns a
//     shared.ErrInvalidConfig error naming the table, the expected schema, and
//     the actual schema so the operator can fix the provisioning.
//
// Expected schema (see CreateTable / doc.go):
//
//	Primary key : PK (S, HASH), SK (S, RANGE)
//	GSI ExpiryIndex   : has_expiry (S, HASH), expires_at (N, RANGE)
//	GSI RecordIDIndex : record_id (S, HASH)
//	GSI ClaimIndex    : PK (S, HASH), claim_sort (S, RANGE), Projection: ALL —
//	                    OPTIONAL; Claim falls back to a whole-partition scan when
//	                    it is absent (c13-claim-quadratic backward-compat), so an
//	                    un-migrated table is not fatal. When PRESENT it must match
//	                    key schema AND be Projection: ALL (the claim query filters
//	                    on the non-key status attribute), so a misprovisioned or
//	                    under-projected ClaimIndex is rejected at startup rather
//	                    than wedging every Claim at runtime.
func (s *Store) Preflight(ctx context.Context) error {
	out, err := s.client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(s.table),
	})
	if err != nil {
		if isResourceNotFound(err) {
			return nil
		}
		return wrapErr(err, "outbox preflight: describe table failed", "table", s.table)
	}

	return validateTableSchema(s.table, "dynamodboutbox", out.Table,
		[]expectedKey{
			{name: "PK", keyType: ddbtypes.KeyTypeHash, attrType: ddbtypes.ScalarAttributeTypeS},
			{name: "SK", keyType: ddbtypes.KeyTypeRange, attrType: ddbtypes.ScalarAttributeTypeS},
		},
		[]expectedIndex{
			{
				name: expiryIndexName,
				keys: []expectedKey{
					{name: attrHasExpiry, keyType: ddbtypes.KeyTypeHash, attrType: ddbtypes.ScalarAttributeTypeS},
					{name: "expires_at", keyType: ddbtypes.KeyTypeRange, attrType: ddbtypes.ScalarAttributeTypeN},
				},
			},
			{
				name: recordIDIndexName,
				keys: []expectedKey{
					{name: "record_id", keyType: ddbtypes.KeyTypeHash, attrType: ddbtypes.ScalarAttributeTypeS},
				},
			},
			{
				// ClaimIndex is optional: Claim degrades to an exhaustive scan
				// when it is absent (c13-claim-quadratic backward-compat), so a
				// pre-migration table stays valid. A present-but-wrong ClaimIndex
				// is still rejected — including a correct key schema with the
				// WRONG projection: the claim query filters on the non-key status
				// attribute, so ClaimIndex MUST be Projection: ALL or every claim
				// query fails at runtime (c13 review HIGH).
				name:              claimIndexName,
				optional:          true,
				wantProjectionAll: true,
				keys: []expectedKey{
					{name: "PK", keyType: ddbtypes.KeyTypeHash, attrType: ddbtypes.ScalarAttributeTypeS},
					{name: attrClaimSort, keyType: ddbtypes.KeyTypeRange, attrType: ddbtypes.ScalarAttributeTypeS},
				},
			},
		},
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

// expectedIndex describes a required global secondary index. When optional is
// true the index may be absent (validateTableSchema skips it), but if present
// it must still match — used for ClaimIndex, whose absence Claim tolerates via
// a scan fallback (c13-claim-quadratic backward-compat). When wantProjectionAll
// is true a PRESENT index must also project ALL attributes: a correctly-keyed
// but under-projected ClaimIndex would pass a key-only check yet fail every
// claim query at runtime (the FilterExpression reads the non-projected `status`
// attribute), wedging the fleet — so preflight rejects it at startup (c13
// review HIGH).
type expectedIndex struct {
	name              string
	optional          bool
	wantProjectionAll bool
	keys              []expectedKey
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
			if want.optional {
				continue // absent optional GSI (e.g. ClaimIndex) is tolerated
			}
			return schemaMismatch(pkg, tableName, fmt.Sprintf(
				"required global secondary index %q is missing (expected key schema %s)",
				want.name, renderExpected(want.keys)))
		}
		if err := checkKeySchema(pkg, tableName, fmt.Sprintf("GSI %q", want.name), got.KeySchema, defs, want.keys); err != nil {
			return err
		}
		if want.wantProjectionAll {
			if got.Projection == nil || got.Projection.ProjectionType != ddbtypes.ProjectionTypeAll {
				return schemaMismatch(pkg, tableName, fmt.Sprintf(
					"GSI %q must be Projection: ALL (the claim query filters on the non-key "+
						"status attribute); actual projection is %q",
					want.name, projectionTypeOf(got)))
			}
		}
	}
	return nil
}

// projectionTypeOf renders a GSI's projection type for a schema-mismatch
// message, tolerating a nil projection description.
func projectionTypeOf(gsi *ddbtypes.GlobalSecondaryIndexDescription) string {
	if gsi == nil || gsi.Projection == nil {
		return "<none>"
	}
	return string(gsi.Projection.ProjectionType)
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
	// a PK-only role pointed at a composite-key table): reject it so the error
	// is symmetric.
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
