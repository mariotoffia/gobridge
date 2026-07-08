package dynamodbdlq

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
// required global secondary indexes match what the DLQ role needs. It is a
// build-time safeguard against a misprovisioned or copy-pasted table name (H3):
// a DLQ pointed at a table lacking RouteIndex/CategoryIndex would fail every
// filtered List/DeleteByFilter at runtime, and one with the wrong primary key
// would corrupt dead-letter storage.
//
// Semantics:
//   - Table missing (ResourceNotFound): NON-fatal, returns nil (build-then-
//     EnsureTable flows stay valid; a missing table fails loudly at first use).
//   - Table present with a mismatched schema: FATAL, returns a
//     shared.ErrInvalidConfig error naming the table, expected, and actual.
//
// Expected schema (see EnsureTable / doc.go):
//
//	Primary key : PK (S, HASH)
//	GSI RouteIndex    : route_id (S, HASH), failed_at (N, RANGE)
//	GSI CategoryIndex : category (S, HASH), failed_at (N, RANGE)
func (s *Store) Preflight(ctx context.Context) error {
	out, err := s.client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(s.tableName),
	})
	if err != nil {
		if isResourceNotFound(err) {
			return nil
		}
		return wrapErr(err, "dlq preflight: describe table failed", "table", s.tableName)
	}

	return validateTableSchema(s.tableName, "dynamodbdlq", out.Table,
		[]expectedKey{
			{name: attrPK, keyType: ddbtypes.KeyTypeHash, attrType: ddbtypes.ScalarAttributeTypeS},
		},
		[]expectedIndex{
			{
				name: "RouteIndex",
				keys: []expectedKey{
					{name: attrRouteID, keyType: ddbtypes.KeyTypeHash, attrType: ddbtypes.ScalarAttributeTypeS},
					{name: attrFailedAt, keyType: ddbtypes.KeyTypeRange, attrType: ddbtypes.ScalarAttributeTypeN},
				},
			},
			{
				name: "CategoryIndex",
				keys: []expectedKey{
					{name: attrCategory, keyType: ddbtypes.KeyTypeHash, attrType: ddbtypes.ScalarAttributeTypeS},
					{name: attrFailedAt, keyType: ddbtypes.KeyTypeRange, attrType: ddbtypes.ScalarAttributeTypeN},
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
