package transport

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// bearerChallenge is the RFC 7235 WWW-Authenticate value returned with
// every 401 from a Bearer-capable endpoint so standard HTTP clients
// learn how to authenticate. The transport accepts the same secret via
// the X-API-Key header or "Authorization: Bearer <key>"; the Bearer
// scheme is advertised as the canonical challenge.
const bearerChallenge = `Bearer realm="gobridge"`

// writeUnauthorized writes a 401 response with the WWW-Authenticate
// challenge. The challenge header MUST be set before the status line is
// written (headers added after WriteHeader are dropped), so it is set
// before delegating to writeError.
func writeUnauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("WWW-Authenticate", bearerChallenge)
	writeError(w, http.StatusUnauthorized, message)
}

// constantTimeSecretMatch reports whether got and want are equal using a
// constant-time comparison over their SHA-256 digests. Hashing first
// keeps the comparison constant-time even when the two values differ in
// length — subtle.ConstantTimeCompare returns 0 immediately for unequal
// slice lengths, which would otherwise leak the secret length.
func constantTimeSecretMatch(got, want string) bool {
	gotHash := sha256.Sum256([]byte(got))
	wantHash := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(gotHash[:], wantHash[:]) == 1
}

// checkAPIKey validates the request API key using constant-time
// comparison against either the X-API-Key header or a Bearer token. See
// constantTimeSecretMatch for the length-leak rationale.
func checkAPIKey(r *http.Request, key string) bool {
	if got := r.Header.Get("X-API-Key"); len(got) > 0 && constantTimeSecretMatch(got, key) {
		return true
	}
	// Auth-scheme comparison is case-insensitive per RFC 7235 §2.1
	// ("bearer", "BEARER" and "Bearer" name the same scheme).
	const bearerPrefix = "Bearer "
	if auth := r.Header.Get("Authorization"); len(auth) > len(bearerPrefix) &&
		strings.EqualFold(auth[:len(bearerPrefix)], bearerPrefix) {
		token := auth[len(bearerPrefix):]
		if constantTimeSecretMatch(token, key) {
			return true
		}
	}
	return false
}

func generateClientID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func sanitizeSSEField(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
}
