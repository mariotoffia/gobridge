package transport

import (
	"crypto/rand"
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

func checkAPIKey(r *http.Request, key string) bool {
	if got := r.Header.Get("X-API-Key"); len(got) > 0 && subtle.ConstantTimeCompare([]byte(got), []byte(key)) == 1 {
		return true
	}
	bearer := "Bearer " + key
	if got := r.Header.Get("Authorization"); len(got) > 0 && subtle.ConstantTimeCompare([]byte(got), []byte(bearer)) == 1 {
		return true
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
