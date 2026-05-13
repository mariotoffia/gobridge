package route

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"strconv"
	"sync/atomic"

	"github.com/mariotoffia/gobridge/domain/clock"
)

// idFallbackCounter feeds the deterministic-but-unique fallback path
// in [generateID] when crypto/rand fails after composition
// (effectively impossible on supported OSes but kept as defence-in-
// depth so the runtime never panics on ID generation). It mirrors the
// runtime/dlq leaf's counter; each leaf owns its own counter so the
// route package never reaches back into its parent for utility state.
//
//nolint:gochecknoglobals // counter must outlive every call
var idFallbackCounter atomic.Uint64

// generateID returns a 32-hex-character identifier sourced from
// crypto/rand. On the unsupported-OS failure path it falls back to a
// timestamp+counter composition so the runtime never panics on ID
// generation. The parent runtime's CheckRandSource probe is the
// supported way to surface a missing entropy source as a structured
// error at startup.
func generateID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return fallbackID()
	}
	return hex.EncodeToString(b)
}

// fallbackID returns a 32-hex-character ID assembled from the
// nanosecond clock (via clock.System, the documented default) and a
// process-local atomic counter. It is reachable only when crypto/rand
// fails post-composition.
func fallbackID() string {
	n := idFallbackCounter.Add(1)
	ts := uint64(clock.System.Now().UnixNano())
	return strconv.FormatUint(ts, 16) + "-" + strconv.FormatUint(n, 16)
}
