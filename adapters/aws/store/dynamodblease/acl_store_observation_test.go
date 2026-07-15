package dynamodblease

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
)

type observationMemoryClient struct {
	mu                     sync.Mutex
	item                   map[string]ddbtypes.AttributeValue
	getErr                 error
	updateErr              error
	loseNextObservationCAS bool
}

func (c *observationMemoryClient) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.item != nil {
		return nil, &ddbtypes.ConditionalCheckFailedException{}
	}
	c.item = cloneObservationItem(in.Item)
	return &dynamodb.PutItemOutput{}, nil
}
func (c *observationMemoryClient) GetItem(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.getErr != nil {
		err := c.getErr
		c.getErr = nil
		return nil, err
	}
	return &dynamodb.GetItemOutput{Item: cloneObservationItem(c.item)}, nil
}
func (c *observationMemoryClient) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.updateErr != nil {
		err := c.updateErr
		c.updateErr = nil
		return nil, err
	}
	expr := ""
	if in.UpdateExpression != nil {
		expr = *in.UpdateExpression
	}
	if strings.Contains(expr, "#obs_elapsed") && c.loseNextObservationCAS {
		c.loseNextObservationCAS = false
		elapsed, _ := observationNumber(c.item, attrObservationElapsed)
		generation, _ := observationNumber(c.item, attrObservationGeneration)
		c.item[attrObservationElapsed] = &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(elapsed+int64(time.Second), 10)}
		c.item[attrObservationGeneration] = &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(generation+1, 10)}
		return nil, &ddbtypes.ConditionalCheckFailedException{}
	}
	if in.ConditionExpression != nil && !observationCondition(c.item, *in.ConditionExpression, in.ExpressionAttributeNames, in.ExpressionAttributeValues) {
		return nil, &ddbtypes.ConditionalCheckFailedException{}
	}
	applyObservationUpdate(c.item, in)
	return &dynamodb.UpdateItemOutput{Attributes: cloneObservationItem(c.item)}, nil
}
func (*observationMemoryClient) CreateTable(context.Context, *dynamodb.CreateTableInput, ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error) {
	return &dynamodb.CreateTableOutput{}, nil
}
func (*observationMemoryClient) DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	return &dynamodb.DescribeTableOutput{}, nil
}
func (*observationMemoryClient) DescribeTimeToLive(context.Context, *dynamodb.DescribeTimeToLiveInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTimeToLiveOutput, error) {
	return &dynamodb.DescribeTimeToLiveOutput{}, nil
}

func cloneObservationItem(in map[string]ddbtypes.AttributeValue) map[string]ddbtypes.AttributeValue {
	if in == nil {
		return nil
	}
	out := make(map[string]ddbtypes.AttributeValue, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
func observationNumber(item map[string]ddbtypes.AttributeValue, key string) (int64, bool) {
	v, ok := item[key].(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(v.Value, 10, 64)
	return n, err == nil
}
func observationCondition(item map[string]ddbtypes.AttributeValue, condition string, names map[string]string, values map[string]ddbtypes.AttributeValue) bool {
	for _, raw := range strings.Split(condition, " AND ") {
		clause := strings.TrimSpace(raw)
		if strings.HasPrefix(clause, "attribute_not_exists(") {
			alias := strings.TrimSuffix(strings.TrimPrefix(clause, "attribute_not_exists("), ")")
			if _, ok := item[names[alias]]; ok {
				return false
			}
			continue
		}
		op := " = "
		if strings.Contains(clause, " >= ") {
			op = " >= "
		}
		parts := strings.Split(clause, op)
		if len(parts) != 2 {
			return false
		}
		actual, aok := item[names[strings.TrimSpace(parts[0])]]
		expected, eok := values[strings.TrimSpace(parts[1])]
		if !aok || !eok {
			return false
		}
		an, anok := actual.(*ddbtypes.AttributeValueMemberN)
		en, enok := expected.(*ddbtypes.AttributeValueMemberN)
		if op == " >= " {
			if !anok || !enok {
				return false
			}
			ai, ae := strconv.ParseInt(an.Value, 10, 64)
			ei, ee := strconv.ParseInt(en.Value, 10, 64)
			if ae != nil || ee != nil || ai < ei {
				return false
			}
			continue
		}
		if anok && enok {
			if an.Value != en.Value {
				return false
			}
			continue
		}
		as, asok := actual.(*ddbtypes.AttributeValueMemberS)
		es, esok := expected.(*ddbtypes.AttributeValueMemberS)
		if !asok || !esok || as.Value != es.Value {
			return false
		}
	}
	return true
}
func applyObservationUpdate(item map[string]ddbtypes.AttributeValue, in *dynamodb.UpdateItemInput) {
	expr := ""
	if in.UpdateExpression != nil {
		expr = *in.UpdateExpression
	}
	names, values := in.ExpressionAttributeNames, in.ExpressionAttributeValues
	set := func(alias, value string) {
		if attr, ok := names[alias]; ok {
			item[attr] = values[value]
		}
	}
	if strings.Contains(expr, "#obs_fp = :next_obs_fp") {
		set("#obs_fp", ":next_obs_fp")
		set("#obs_elapsed", ":next_obs_elapsed")
		set("#obs_gen", ":next_obs_gen")
	}
	if strings.Contains(expr, "#own = :owner") {
		set("#own", ":owner")
	}
	if strings.Contains(expr, "#own = :empty") {
		set("#own", ":empty")
	}
	if strings.Contains(expr, "#acq = :now_ms") {
		set("#acq", ":now_ms")
	}
	if strings.Contains(expr, "#exp = :exp_ms") {
		set("#exp", ":exp_ms")
	}
	if strings.Contains(expr, "#exp = :zero") {
		set("#exp", ":zero")
	}
	if strings.Contains(expr, "#ren = :now_ms") {
		set("#ren", ":now_ms")
	}
	if strings.Contains(expr, "#ver = if_not_exists") || strings.Contains(expr, "#ver = #ver + :one") {
		version, _ := observationNumber(item, attrVersion)
		item[attrVersion] = &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(version+1, 10)}
	}
	if strings.Contains(expr, "REMOVE") {
		for alias, attr := range names {
			if strings.Contains(expr, alias) && (strings.HasPrefix(alias, "#obs_") || alias == "#ttl") {
				delete(item, attr)
			}
		}
	}
}
func seededObservationClient(now time.Time, ttl time.Duration, legacy bool) *observationMemoryClient {
	item := map[string]ddbtypes.AttributeValue{attrPK: &ddbtypes.AttributeValueMemberS{Value: leaseKey("lease-1")}, attrOwner: &ddbtypes.AttributeValueMemberS{Value: "owner-a"}, attrVersion: &ddbtypes.AttributeValueMemberN{Value: "7"}, attrExpiresAt: &ddbtypes.AttributeValueMemberN{Value: millisStr(now.Add(ttl))}}
	if !legacy {
		item[attrRenewedAt] = &ddbtypes.AttributeValueMemberN{Value: millisStr(now)}
	}
	return &observationMemoryClient{item: item}
}
func persistedObservationElapsed(t *testing.T, c *observationMemoryClient) time.Duration {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	n, _ := observationNumber(c.item, attrObservationElapsed)
	return time.Duration(n)
}
func persistedObservationPresent(c *observationMemoryClient) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.item[attrObservationFingerprint]
	return ok
}

func TestAcquire_PersistedObservationInheritedByReplacement(t *testing.T) {
	const ttl = 10 * time.Second
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client := seededObservationClient(base, ttl, false)
	clockA := clocktest.NewAt(base.Add(-24 * time.Hour))
	storeA := &Store{client: client, tableName: "leases", clk: clockA}
	_, err := storeA.Acquire(t.Context(), "lease-1", "standby-a", ttl, nil)
	if !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("first observation: %v", err)
	}
	clockA.Advance(6 * time.Second)
	_, err = storeA.Acquire(t.Context(), "lease-1", "standby-a", ttl, nil)
	if !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("partial observation: %v", err)
	}
	if got := persistedObservationElapsed(t, client); got != 6*time.Second {
		t.Fatalf("elapsed=%s want=6s", got)
	}
	clockB := clocktest.NewAt(base.Add(100 * 365 * 24 * time.Hour))
	storeB := &Store{client: client, tableName: "leases", clk: clockB}
	_, _ = storeB.Acquire(t.Context(), "lease-1", "standby-b", ttl, nil)
	clockB.Advance(4 * time.Second)
	token, err := storeB.Acquire(t.Context(), "lease-1", "standby-b", ttl, nil)
	if err != nil {
		t.Fatalf("replacement takeover: %v", err)
	}
	if token.Owner != "standby-b" || token.Version != 8 {
		t.Fatalf("token=%+v", token)
	}
	if persistedObservationPresent(client) {
		t.Fatal("takeover retained evidence")
	}
}

func TestAcquire_CompetingObserversDoNotDoubleCountOverlap(t *testing.T) {
	const ttl = 20 * time.Second
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client := seededObservationClient(base, ttl, false)
	ac, bc := clocktest.NewAt(base), clocktest.NewAt(base.Add(48*time.Hour))
	a := &Store{client: client, tableName: "leases", clk: ac}
	b := &Store{client: client, tableName: "leases", clk: bc}
	_, _ = a.Acquire(t.Context(), "lease-1", "standby-a", ttl, nil)
	_, _ = b.Acquire(t.Context(), "lease-1", "standby-b", ttl, nil)
	ac.Advance(6 * time.Second)
	bc.Advance(6 * time.Second)
	_, _ = a.Acquire(t.Context(), "lease-1", "standby-a", ttl, nil)
	_, _ = b.Acquire(t.Context(), "lease-1", "standby-b", ttl, nil)
	if got := persistedObservationElapsed(t, client); got != 6*time.Second {
		t.Fatalf("overlap double counted: %s", got)
	}
}

func TestAcquire_ObservationCASLossDiscardsLocalElapsed(t *testing.T) {
	const ttl = 20 * time.Second
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client := seededObservationClient(base, ttl, false)
	clk := clocktest.NewAt(base)
	store := &Store{client: client, tableName: "leases", clk: clk}
	_, _ = store.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
	clk.Advance(5 * time.Second)
	client.loseNextObservationCAS = true
	_, err := store.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
	if !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("CAS loss=%v", err)
	}
	_, _ = store.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
	clk.Advance(5 * time.Second)
	_, _ = store.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
	if got := persistedObservationElapsed(t, client); got != 6*time.Second {
		t.Fatalf("stale interval reused: %s", got)
	}
}

func TestAcquire_LivenessTupleAndLegacyMutationsResetEvidence(t *testing.T) {
	mutations := map[string]func(map[string]ddbtypes.AttributeValue){"owner": func(i map[string]ddbtypes.AttributeValue) {
		i[attrOwner] = &ddbtypes.AttributeValueMemberS{Value: "owner-b"}
	}, "version": func(i map[string]ddbtypes.AttributeValue) {
		i[attrVersion] = &ddbtypes.AttributeValueMemberN{Value: "8"}
	}, "renewed": func(i map[string]ddbtypes.AttributeValue) {
		i[attrRenewedAt] = &ddbtypes.AttributeValueMemberN{Value: "1767225601000"}
	}, "expires": func(i map[string]ddbtypes.AttributeValue) {
		i[attrExpiresAt] = &ddbtypes.AttributeValueMemberN{Value: "1767225621000"}
	}}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			const ttl = 20 * time.Second
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			client := seededObservationClient(base, ttl, false)
			clk := clocktest.NewAt(base)
			store := &Store{client: client, tableName: "leases", clk: clk}
			_, _ = store.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
			clk.Advance(10 * time.Second)
			_, _ = store.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
			client.mu.Lock()
			mutate(client.item)
			client.mu.Unlock()
			clk.Advance(10 * time.Second)
			_, err := store.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
			if !errors.Is(err, shared.ErrAlreadyExists) {
				t.Fatalf("mutation takeover: %v", err)
			}
			if got := persistedObservationElapsed(t, client); got != 0 {
				t.Fatalf("retained=%s", got)
			}
		})
	}

}

func TestAcquire_ReadWriteFailureAndClockRegressionRestart(t *testing.T) {
	const ttl = 30 * time.Second
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client := seededObservationClient(base, ttl, false)
	clk := clocktest.NewAt(base)
	store := &Store{client: client, tableName: "leases", clk: clk}
	_, _ = store.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
	clk.Advance(5 * time.Second)
	client.getErr = errors.New("read")
	if _, err := store.Acquire(t.Context(), "lease-1", "standby", ttl, nil); err == nil {
		t.Fatal("read failure accepted")
	}
	_, _ = store.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
	clk.Advance(5 * time.Second)
	client.updateErr = errors.New("write")
	if _, err := store.Acquire(t.Context(), "lease-1", "standby", ttl, nil); err == nil {
		t.Fatal("write failure accepted")
	}
	_, _ = store.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
	clk.Advance(-time.Second)
	_, err := store.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
	if !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("regression=%v", err)
	}
	clk.Advance(5 * time.Second)
	_, _ = store.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
	if got := persistedObservationElapsed(t, client); got != 5*time.Second {
		t.Fatalf("discard failed: %s", got)
	}
}

func TestRenewReleaseTakeoverResetObservation(t *testing.T) {
	const ttl = 20 * time.Second
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, op := range []string{"renew", "release", "takeover"} {
		t.Run(op, func(t *testing.T) {
			client := seededObservationClient(base, ttl, false)
			clk := clocktest.NewAt(base)
			standby := &Store{client: client, tableName: "leases", clk: clk}
			_, _ = standby.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
			owner := &Store{client: client, tableName: "leases", clk: clk}
			token := persistence.LeaseToken{Owner: "owner-a", Version: 7}
			switch op {
			case "renew":
				_, _ = owner.Renew(t.Context(), "lease-1", token, ttl, nil)
			case "release":
				_ = owner.Release(t.Context(), "lease-1", token)
			case "takeover":
				clk.Advance(ttl)
				_, _ = standby.Acquire(t.Context(), "lease-1", "standby", ttl, nil)
			}
			if persistedObservationPresent(client) {
				t.Fatalf("%s retained evidence", op)
			}
		})
	}
}

func TestAcquire_MalformedOverflowAndSkewFailClosed(t *testing.T) {
	const ttl = 20 * time.Second
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for name, value := range map[string]string{"negative": "-1", "overflow": "9223372036854775808"} {
		t.Run(name, func(t *testing.T) {
			client := seededObservationClient(base, ttl, false)
			client.item[attrObservationFingerprint] = &ddbtypes.AttributeValueMemberS{Value: "fp"}
			client.item[attrObservationElapsed] = &ddbtypes.AttributeValueMemberN{Value: value}
			client.item[attrObservationGeneration] = &ddbtypes.AttributeValueMemberN{Value: "1"}
			store := &Store{client: client, tableName: "leases", clk: clocktest.NewAt(base)}
			if _, err := store.Acquire(t.Context(), "lease-1", "standby", ttl, nil); err == nil || errors.Is(err, shared.ErrAlreadyExists) {
				t.Fatalf("malformed accepted: %v", err)
			}
		})
	}
	if got := saturatingObservationDuration(time.Duration(math.MaxInt64-1), 10); got != time.Duration(math.MaxInt64) {
		t.Fatalf("saturation=%d", got)
	}
	client := seededObservationClient(base, 10*time.Second, false)
	clk := clocktest.NewAt(base.Add(200 * 365 * 24 * time.Hour))
	store := &Store{client: client, tableName: "leases", clk: clk}
	_, err := store.Acquire(t.Context(), "lease-1", "standby", 10*time.Second, nil)
	if !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("first skew=%v", err)
	}
	clk.Advance(10*time.Second - time.Nanosecond)
	_, err = store.Acquire(t.Context(), "lease-1", "standby", 10*time.Second, nil)
	if !errors.Is(err, shared.ErrAlreadyExists) {
		t.Fatalf("early skew takeover=%v", err)
	}
}
