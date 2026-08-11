package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/runtime"
)

// TestThrottleKeyFromRequest_IgnoresXFF pins the throttle key contract: it is
// the RemoteAddr host (transport peer, port stripped) and NEVER incorporates
// the client-controlled X-Forwarded-For header.
func TestThrottleKeyFromRequest_IgnoresXFF(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "203.0.113.7:54321"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")
	assert.Equal(t, "203.0.113.7", throttleKeyFromRequest(req))

	// Rotating the (spoofable) XFF must not change the key.
	req.Header.Set("X-Forwarded-For", "9.9.9.9")
	assert.Equal(t, "203.0.113.7", throttleKeyFromRequest(req))

	// A RemoteAddr with no port (unusual synthetic request) is used verbatim
	// rather than dropping the identity.
	req.RemoteAddr = "unixsocket"
	assert.Equal(t, "unixsocket", throttleKeyFromRequest(req))
}

// TestRequireAdminAuth_ThrottleKeyedOnRemoteAddr_NotXFF is the regression:
// an attacker rotating X-Forwarded-For on every request from one transport peer
// must STILL be throttled. The pre-fix code keyed the limiter on the spoofable
// leftmost XFF, so each rotated value looked like a new client and reset the
// counter — defeating the limiter entirely (and, under an AWS ALB that APPENDS
// to client XFF, this held in the shipped topology). It also verifies:
// Retry-After is derived from AuthFailureWindow, not the old hardcoded 60.
func TestRequireAdminAuth_ThrottleKeyedOnRemoteAddr_NotXFF(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	rt := runtime.New(runtime.WithInstanceID("auth-throttle-xff"))
	cfg := testConfig()
	cfg.AuthFailureLimit = 3
	cfg.AuthFailureWindow = 90 * time.Second
	s := New(rt, cfg, WithClock(clk))

	h := s.requireAdminAuth(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	attempt := 0
	do := func() *httptest.ResponseRecorder {
		attempt++
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
		req.Header.Set("X-API-Key", "wrong")
		req.RemoteAddr = "10.0.0.9:1234" // same transport peer every time
		// Rotate XFF on every request: with the pre-fix key this reset the
		// counter and the client was never throttled.
		req.Header.Set("X-Forwarded-For", "198.51.100."+strconv.Itoa(attempt))
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}

	for i := 0; i < 3; i++ {
		assert.Equal(t, http.StatusUnauthorized, do().Code)
	}
	// 4th attempt from the same RemoteAddr is throttled despite the rotated XFF.
	rec := do()
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)

	// Retry-After tracks AuthFailureWindow (90s -> "90"), not "60".
	assert.Equal(t, "90", rec.Header().Get("Retry-After"))
}

// TestRequireAdminAuth_XFFSpoofDoesNotThrottleVictim proves an attacker cannot
// weaponise X-Forwarded-For against the limiter: a peer that forges the SAME
// leftmost XFF as a victim is tracked under its OWN RemoteAddr, so the victim's
// counter is untouched and they are not locked out.
func TestRequireAdminAuth_XFFSpoofDoesNotThrottleVictim(t *testing.T) {
	clk := clocktest.NewAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	rt := runtime.New(runtime.WithInstanceID("auth-throttle-spoof"))
	cfg := testConfig()
	cfg.AuthFailureLimit = 3
	cfg.AuthFailureWindow = time.Minute
	s := New(rt, cfg, WithClock(clk))

	h := s.requireAdminAuth(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Attacker peer forges the victim's IP in XFF and exhausts the limit.
	for i := 0; i < 4; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
		req.Header.Set("X-API-Key", "wrong")
		req.RemoteAddr = "10.0.0.66:5555"             // attacker's transport peer
		req.Header.Set("X-Forwarded-For", "10.0.0.9") // victim's IP forged
		rec := httptest.NewRecorder()
		h(rec, req)
	}

	// The victim, on their real transport peer, is NOT throttled: the attacker's
	// spoofed XFF never touched the victim's RemoteAddr-keyed counter.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bridge", nil)
	req.Header.Set("X-API-Key", "test-secret-key-0123456789")
	req.RemoteAddr = "10.0.0.9:1234" // victim's real peer
	rec := httptest.NewRecorder()
	h(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}
