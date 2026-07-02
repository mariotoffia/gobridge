package dynamodbdlq

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
)

type fakeDLQClient struct {
	putItems  []*dynamodb.PutItemInput
	scanCalls int
	// scanFn optionally shapes the paginated Scan responses.
	scanFn func(int, *dynamodb.ScanInput) (*dynamodb.ScanOutput, error)
}

func (f *fakeDLQClient) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.putItems = append(f.putItems, in)
	return &dynamodb.PutItemOutput{}, nil
}

func (f *fakeDLQClient) GetItem(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return &dynamodb.GetItemOutput{}, nil
}

func (f *fakeDLQClient) Query(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	return &dynamodb.QueryOutput{}, nil
}

func (f *fakeDLQClient) Scan(_ context.Context, in *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	f.scanCalls++
	if f.scanFn != nil {
		return f.scanFn(f.scanCalls, in)
	}
	return &dynamodb.ScanOutput{}, nil
}

func (f *fakeDLQClient) DeleteItem(_ context.Context, _ *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	return &dynamodb.DeleteItemOutput{}, nil
}

func (f *fakeDLQClient) CreateTable(_ context.Context, _ *dynamodb.CreateTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error) {
	return &dynamodb.CreateTableOutput{}, nil
}

func (f *fakeDLQClient) DescribeTable(_ context.Context, _ *dynamodb.DescribeTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return &dynamodb.DescribeTableOutput{}, nil
}

func (f *fakeDLQClient) UpdateTimeToLive(_ context.Context, _ *dynamodb.UpdateTimeToLiveInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateTimeToLiveOutput, error) {
	return &dynamodb.UpdateTimeToLiveOutput{}, nil
}

func testEntry(id string) routing.DLQEntry {
	return routing.NewDLQEntry(routing.DLQEntrySpec{
		ID: id,
		Envelope: *messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "env-" + id,
			Subject: "test/subject",
			Payload: []byte(`{"k":"v"}`),
		}),
		RouteID:  "route-" + id,
		Category: "schema",
		FailedAt: time.Unix(1_700_000_000, 0),
		Attempts: 3,
	})
}

func newDLQStore(f *fakeDLQClient, opts ...Option) *Store {
	s := &Store{client: f, tableName: "dlq-test", maxScanPages: defaultMaxScanPages}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Regression for J7: DLQ entries must not carry a TTL by default, so a
// dead-lettered message is not racing a short expiry clock during
// investigation.
func TestWrite_NoTTLByDefault(t *testing.T) {
	f := &fakeDLQClient{}
	s := newDLQStore(f)

	if err := s.Write(context.Background(), testEntry("e1")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(f.putItems) != 1 {
		t.Fatalf("expected 1 PutItem, got %d", len(f.putItems))
	}
	if _, ok := f.putItems[0].Item[attrTTL]; ok {
		t.Fatalf("default write stamped a %q attribute; DLQ must default to no TTL", attrTTL)
	}
}

// Regression for J7: an explicit retention window opts into TTL with a
// days-scale (or configured) expiry = failed_at + retention.
func TestWrite_TTLWhenRetentionConfigured(t *testing.T) {
	f := &fakeDLQClient{}
	s := newDLQStore(f, WithRetention(72*time.Hour))

	entry := testEntry("e1")
	if err := s.Write(context.Background(), entry); err != nil {
		t.Fatalf("write: %v", err)
	}
	ttl, ok := f.putItems[0].Item[attrTTL].(*ddbtypes.AttributeValueMemberN)
	if !ok {
		t.Fatalf("expected a %q attribute when retention configured", attrTTL)
	}
	want := i64(entry.FailedAt().Add(72 * time.Hour).Unix())
	if ttl.Value != want {
		t.Fatalf("ttl = %s, want %s (failed_at + retention)", ttl.Value, want)
	}
}

// Regression for J11: an index-less Purge must not scan the table unbounded;
// it stops after WithMaxScanPages pages.
func TestPurge_BoundedByMaxScanPages(t *testing.T) {
	f := &fakeDLQClient{}
	// Every page reports more data (never nil LastEvaluatedKey) and no items.
	f.scanFn = func(_ int, _ *dynamodb.ScanInput) (*dynamodb.ScanOutput, error) {
		return &dynamodb.ScanOutput{
			LastEvaluatedKey: map[string]ddbtypes.AttributeValue{
				attrPK: &ddbtypes.AttributeValueMemberS{Value: "cursor"},
			},
		}, nil
	}
	s := newDLQStore(f, WithMaxScanPages(3))

	if _, err := s.Purge(context.Background(), time.Now()); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if f.scanCalls != 3 {
		t.Fatalf("purge should stop after 3 scan pages, got %d", f.scanCalls)
	}
}
