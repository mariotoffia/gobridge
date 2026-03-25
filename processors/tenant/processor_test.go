package tenant

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

func nextOK(_ context.Context, _ *domain.Envelope) error { return nil }

func envelope(tenantID string, payloadSize int) *domain.Envelope {
	env := &domain.Envelope{Subject: "test"}
	if tenantID != "" {
		env.Headers = map[string]any{domain.HeaderTenantID: tenantID}
	}
	if payloadSize > 0 {
		env.Payload = make([]byte, payloadSize)
	}
	return env
}

type stubValidator struct {
	info ports.TenantInfo
	err  error
}

func (s *stubValidator) Validate(_ context.Context, _ string) (ports.TenantInfo, error) {
	return s.info, s.err
}

type stubTracker struct {
	messages  atomic.Int64
	inFlight  atomic.Int64
	failOnMsg bool
	failOnIF  bool
}

func (s *stubTracker) IncrementMessages(_ context.Context, _ string, count int64) error {
	if s.failOnMsg {
		return errors.New("message tracking failed")
	}
	s.messages.Add(count)
	return nil
}

func (s *stubTracker) IncrementInFlight(_ context.Context, _ string, delta int64) error {
	if s.failOnIF {
		return errors.New("in-flight tracking failed")
	}
	s.inFlight.Add(delta)
	return nil
}

// Verifies processing succeeds when tenant header is absent and not required.
func TestProcess_NoTenantHeader_NotRequired(t *testing.T) {
	p := New(Config{})
	env := &domain.Envelope{Subject: "test"}
	if err := p.Process(context.Background(), env, nextOK); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// Verifies missing tenant returns ErrInvalidPayload when required.
func TestProcess_NoTenantHeader_Required(t *testing.T) {
	p := New(Config{RequireTenant: true})
	env := &domain.Envelope{Subject: "test"}
	err := p.Process(context.Background(), env, nextOK)
	if err == nil {
		t.Fatal("expected error for missing tenant")
	}
	if !errors.Is(err, domain.ErrInvalidPayload) {
		t.Errorf("expected ErrInvalidPayload, got %v", err)
	}
}

// Verifies an active validated tenant invokes next.
func TestProcess_ValidTenant_PassesThrough(t *testing.T) {
	v := &stubValidator{info: ports.TenantInfo{ID: "acme", Active: true}}
	p := New(Config{}, WithValidator(v))
	env := envelope("acme", 0)

	called := false
	err := p.Process(context.Background(), env, func(_ context.Context, _ *domain.Envelope) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("expected next to be called")
	}
}

// Verifies inactive tenant is rejected with ErrInvalidPayload.
func TestProcess_InactiveTenant_Rejected(t *testing.T) {
	v := &stubValidator{info: ports.TenantInfo{ID: "acme", Active: false}}
	p := New(Config{}, WithValidator(v))
	env := envelope("acme", 0)

	err := p.Process(context.Background(), env, nextOK)
	if !errors.Is(err, domain.ErrInvalidPayload) {
		t.Errorf("expected ErrInvalidPayload, got %v", err)
	}
}

// Verifies validator errors propagate.
func TestProcess_ValidationError_Propagated(t *testing.T) {
	sentinel := errors.New("db connection lost")
	v := &stubValidator{err: sentinel}
	p := New(Config{}, WithValidator(v))
	env := envelope("acme", 0)

	err := p.Process(context.Background(), env, nextOK)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected wrapped sentinel, got %v", err)
	}
}

// Verifies oversized payload is rejected against tenant quota.
func TestProcess_MessageSizeExceedsQuota(t *testing.T) {
	v := &stubValidator{info: ports.TenantInfo{
		ID: "acme", Active: true, MaxMessageSizeBytes: 100,
	}}
	p := New(Config{}, WithValidator(v))
	env := envelope("acme", 200)

	err := p.Process(context.Background(), env, nextOK)
	if !errors.Is(err, domain.ErrInvalidPayload) {
		t.Errorf("expected ErrInvalidPayload for oversized message, got %v", err)
	}
}

// Verifies payload within MaxMessageSizeBytes passes.
func TestProcess_MessageSizeWithinQuota(t *testing.T) {
	v := &stubValidator{info: ports.TenantInfo{
		ID: "acme", Active: true, MaxMessageSizeBytes: 100,
	}}
	p := New(Config{}, WithValidator(v))
	env := envelope("acme", 50)

	if err := p.Process(context.Background(), env, nextOK); err != nil {
		t.Fatalf("expected nil for message within quota, got %v", err)
	}
}

// Verifies zero quota skips size enforcement.
func TestProcess_ZeroQuota_NoSizeCheck(t *testing.T) {
	v := &stubValidator{info: ports.TenantInfo{
		ID: "acme", Active: true, MaxMessageSizeBytes: 0,
	}}
	p := New(Config{}, WithValidator(v))
	env := envelope("acme", 99999)

	if err := p.Process(context.Background(), env, nextOK); err != nil {
		t.Fatalf("expected nil when quota is zero (unlimited), got %v", err)
	}
}

// Verifies usage tracker increments messages and balances in-flight.
func TestProcess_UsageTracker_InFlightAndMessages(t *testing.T) {
	tracker := &stubTracker{}
	p := New(Config{}, WithUsageTracker(tracker))
	env := envelope("acme", 0)

	if err := p.Process(context.Background(), env, nextOK); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := tracker.inFlight.Load(); got != 0 {
		t.Errorf("in-flight = %d, want 0 (incremented then decremented)", got)
	}
	if got := tracker.messages.Load(); got != 1 {
		t.Errorf("messages = %d, want 1", got)
	}
}

// Verifies message count is skipped on next error but in-flight is decremented.
func TestProcess_UsageTracker_NoMessageCountOnError(t *testing.T) {
	tracker := &stubTracker{}
	p := New(Config{}, WithUsageTracker(tracker))
	env := envelope("acme", 0)

	sentinel := errors.New("downstream failure")
	err := p.Process(context.Background(), env, func(_ context.Context, _ *domain.Envelope) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel, got %v", err)
	}

	if got := tracker.messages.Load(); got != 0 {
		t.Errorf("messages = %d, want 0 (should not count on error)", got)
	}
	if got := tracker.inFlight.Load(); got != 0 {
		t.Errorf("in-flight = %d, want 0 (should decrement even on error)", got)
	}
}

// Verifies in-flight increment failure prevents calling next.
func TestProcess_InFlightTrackingError_ReturnedBeforeNext(t *testing.T) {
	tracker := &stubTracker{failOnIF: true}
	p := New(Config{}, WithUsageTracker(tracker))
	env := envelope("acme", 0)

	called := false
	err := p.Process(context.Background(), env, func(_ context.Context, _ *domain.Envelope) error {
		called = true
		return nil
	})
	if err == nil {
		t.Fatal("expected error from in-flight tracking")
	}
	if called {
		t.Error("next should not be called when in-flight tracking fails")
	}
}

// Verifies custom tenant header name is read for validation.
func TestProcess_CustomTenantHeader(t *testing.T) {
	v := &stubValidator{info: ports.TenantInfo{ID: "custom-tenant", Active: true}}
	p := New(Config{TenantHeader: "x-custom-tenant"}, WithValidator(v))
	env := &domain.Envelope{
		Subject: "test",
		Headers: map[string]any{"x-custom-tenant": "custom-tenant"},
	}

	if err := p.Process(context.Background(), env, nextOK); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Verifies next-handler errors propagate.
func TestProcess_NextErrorPropagation(t *testing.T) {
	p := New(Config{})
	env := envelope("acme", 0)

	sentinel := errors.New("downstream failure")
	err := p.Process(context.Background(), env, func(_ context.Context, _ *domain.Envelope) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel, got %v", err)
	}
}

// Verifies Name default and configured value.
func TestProcessor_Name(t *testing.T) {
	t.Run("configured name", func(t *testing.T) {
		p := New(Config{Name: "my-tenant"})
		if got := p.Name(); got != "my-tenant" {
			t.Errorf("Name() = %q, want %q", got, "my-tenant")
		}
	})
	t.Run("default name", func(t *testing.T) {
		p := New(Config{})
		if got := p.Name(); got != "tenant" {
			t.Errorf("Name() = %q, want %q", got, "tenant")
		}
	})
}

// Verifies next runs without validator when tenant header is present.
func TestProcess_NoValidator_SkipsValidation(t *testing.T) {
	p := New(Config{})
	env := envelope("acme", 0)

	called := false
	err := p.Process(context.Background(), env, func(_ context.Context, _ *domain.Envelope) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("expected next to be called when no validator is set")
	}
}

// Verifies nil headers succeed when tenant is not required.
func TestProcess_NilHeaders_NotRequired(t *testing.T) {
	p := New(Config{})
	env := &domain.Envelope{Subject: "test"}

	if err := p.Process(context.Background(), env, nextOK); err != nil {
		t.Fatalf("expected nil for nil headers (not required), got %v", err)
	}
}

// Verifies validator and usage tracker together on a successful path.
func TestProcess_ValidatorAndTracker_FullFlow(t *testing.T) {
	v := &stubValidator{info: ports.TenantInfo{
		ID: "acme", Active: true, MaxMessageSizeBytes: 1024,
	}}
	tracker := &stubTracker{}
	p := New(Config{RequireTenant: true}, WithValidator(v), WithUsageTracker(tracker))
	env := envelope("acme", 100)

	if err := p.Process(context.Background(), env, nextOK); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := tracker.messages.Load(); got != 1 {
		t.Errorf("messages = %d, want 1", got)
	}
	if got := tracker.inFlight.Load(); got != 0 {
		t.Errorf("in-flight = %d, want 0", got)
	}
}
