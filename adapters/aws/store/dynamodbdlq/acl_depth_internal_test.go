package dynamodbdlq

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// TestDepth_ReadsItemCountMetadata pins the bounded DLQ-depth path: Depth serves
// the outstanding-entry count from DynamoDB's maintained TableDescription
// item-count metadata via a single DescribeTable call — an O(1), no-scan read —
// and NEVER via a full-table Scan.
func TestDepth_ReadsItemCountMetadata(t *testing.T) {
	f := &fakeDLQClient{}
	f.describeTableFn = func(in *dynamodb.DescribeTableInput) (*dynamodb.DescribeTableOutput, error) {
		return &dynamodb.DescribeTableOutput{
			Table: &ddbtypes.TableDescription{ItemCount: aws.Int64(42)},
		}, nil
	}
	s := newDLQStore(f)

	n, err := s.Depth(context.Background())
	if err != nil {
		t.Fatalf("Depth: %v", err)
	}
	if n != 42 {
		t.Fatalf("Depth = %d; want 42 (from ItemCount metadata)", n)
	}
	if f.describeCalls != 1 {
		t.Fatalf("Depth DescribeTable calls = %d; want 1 (bounded metadata read)", f.describeCalls)
	}
	if f.scanCalls != 0 {
		t.Fatalf("Depth must not Scan the table; Scan calls = %d", f.scanCalls)
	}
}

// TestDepth_NilItemCount_ReturnsZero: a freshly created table (or DynamoDB Local
// before its first metadata refresh) can report a nil ItemCount; Depth treats a
// missing count as an empty backlog rather than an error.
func TestDepth_NilItemCount_ReturnsZero(t *testing.T) {
	f := &fakeDLQClient{}
	f.describeTableFn = func(*dynamodb.DescribeTableInput) (*dynamodb.DescribeTableOutput, error) {
		return &dynamodb.DescribeTableOutput{Table: &ddbtypes.TableDescription{}}, nil
	}
	s := newDLQStore(f)

	n, err := s.Depth(context.Background())
	if err != nil {
		t.Fatalf("Depth: %v", err)
	}
	if n != 0 {
		t.Fatalf("Depth (nil ItemCount) = %d; want 0", n)
	}
}

// TestDepth_NilTable_ReturnsZero covers the defensive branch where DescribeTable
// returns no Table description at all (never observed against real DynamoDB, but
// a nil-safe read must not panic): Depth treats it as an empty backlog.
func TestDepth_NilTable_ReturnsZero(t *testing.T) {
	f := &fakeDLQClient{}
	f.describeTableFn = func(*dynamodb.DescribeTableInput) (*dynamodb.DescribeTableOutput, error) {
		return &dynamodb.DescribeTableOutput{}, nil
	}
	s := newDLQStore(f)

	n, err := s.Depth(context.Background())
	if err != nil {
		t.Fatalf("Depth: %v", err)
	}
	if n != 0 {
		t.Fatalf("Depth (nil Table) = %d; want 0", n)
	}
}

// TestDepth_BackendError_ReturnedAsIs: a transient DescribeTable failure is
// returned to the caller (never swallowed, never a scan) so runtime.ReportDLQDepth
// treats it as "depth unavailable this sample" and emits nothing.
func TestDepth_BackendError_ReturnedAsIs(t *testing.T) {
	f := &fakeDLQClient{}
	f.describeTableFn = func(*dynamodb.DescribeTableInput) (*dynamodb.DescribeTableOutput, error) {
		return nil, errors.New("InternalServerError: describe failed")
	}
	s := newDLQStore(f)

	n, err := s.Depth(context.Background())
	if err == nil {
		t.Fatal("Depth: expected a backend error, got nil")
	}
	if n != 0 {
		t.Fatalf("Depth on error = %d; want 0", n)
	}
	if f.scanCalls != 0 {
		t.Fatalf("Depth must not fall back to a Scan on error; Scan calls = %d", f.scanCalls)
	}
}
