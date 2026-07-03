package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// errTrailingData signals that a request body carried bytes beyond the
// single JSON value the handler expected -- trailing garbage (e.g.
// `{"ttl":"90s"}JUNK`) or a second JSON value (a multi-object body). It
// is deliberately distinct from io.EOF so that callers which treat an
// empty body as "use defaults" (handleConfigTxnCreate) still reject a
// valid leading value followed by junk.
var errTrailingData = errors.New("httpapi: unexpected trailing data after JSON value")

// decodeStrictJSON decodes exactly one JSON value from r into v and
// rejects any trailing bytes or additional JSON values. A well-formed
// single-value body leaves nothing after the first value, so the
// follow-up Decode must report io.EOF; anything else -- a second value,
// or non-JSON garbage -- is reported as errTrailingData. An empty or
// whitespace-only body surfaces as io.EOF from the first Decode, which
// callers may treat as "no body supplied".
//
// DisallowUnknownFields is enabled: a field the target struct does not
// declare is a hard error rather than a silent drop. This is the enabler
// the config PATCH path needs — an overlay carrying a plugin `options`
// block (which the ports def types tag json:"-") is REJECTED loudly
// instead of being silently discarded and then erased from disk on
// commit.
func decodeStrictJSON(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		// Wrap so wrapcheck is satisfied; %w preserves io.EOF so callers
		// that treat an empty body as "use defaults" still match it.
		return fmt.Errorf("httpapi: decode JSON body: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errTrailingData
	}
	return nil
}
