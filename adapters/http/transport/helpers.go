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

// checkAPIKey validates the request API key using constant-time comparison.
// Both values are SHA256-hashed before comparison to prevent length-based
// timing leaks (subtle.ConstantTimeCompare returns 0 immediately when
// slice lengths differ).
func checkAPIKey(r *http.Request, key string) bool {
	expHash := sha256.Sum256([]byte(key))
	if got := r.Header.Get("X-API-Key"); len(got) > 0 {
		gotHash := sha256.Sum256([]byte(got))
		if subtle.ConstantTimeCompare(gotHash[:], expHash[:]) == 1 {
			return true
		}
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		token := strings.TrimPrefix(auth, "Bearer ")
		tHash := sha256.Sum256([]byte(token))
		if subtle.ConstantTimeCompare(tHash[:], expHash[:]) == 1 {
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
