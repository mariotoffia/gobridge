package transport

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
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
	if got := r.Header.Get("X-API-Key"); got == key {
		return true
	}
	if got := r.Header.Get("Authorization"); got == "Bearer "+key {
		return true
	}
	return false
}

func generateClientID() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return hex.EncodeToString(b)
}
