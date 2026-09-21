package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/mariotoffia/gobridge/ports"
)

// writeContentProjection writes one value — the normalised blueprint, or one
// plugin's decoded options — into out as that value's content projection: JSON
// with every absent and empty collection reduced to the same form by
// ports.WithoutEmptyCollections (ADR 0016), followed by a newline. A value that
// carries nothing at all is written as "null", so it still occupies its position
// in the stream, and the newline keeps the ordered stream of plugin payloads
// self-delimiting — exactly as the json.Encoder this replaced did.
//
// It exists so the configuration manager's fingerprint and the bridge's content
// identity project a config the same way. They answer the same question about
// the same config from different sides, and plugin option structs commonly tag
// their collections without omitempty, so a nil slice is written out as `[]` and
// comes back non-nil. Applying the rule on one side only would leave a member
// that adopted a config decoded from the durable committed artifact reporting a
// change still outstanding that the bridge considers applied.
//
// The value is read back as a tree so that rule can be applied to it, and every
// number in that tree is carried as a json.Number and written out with the
// digits it was written with. Nothing is rounded through float64, which cannot
// tell an int64 option above 2^53 from its neighbour and would fingerprint two
// different configurations identically.
//
// Every failure is returned: a config that cannot be projected has no identity,
// and the caller must fail closed rather than hash nothing.
func writeContentProjection(out io.Writer, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("render the content projection: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return fmt.Errorf("re-read the content projection: %w", err)
	}
	projected := []byte("null")
	if reduced, keep := ports.WithoutEmptyCollections(tree); keep {
		if projected, err = json.Marshal(reduced); err != nil {
			return fmt.Errorf("render the reduced content projection: %w", err)
		}
	}
	if _, err := out.Write(append(projected, '\n')); err != nil {
		return fmt.Errorf("write the content projection: %w", err)
	}
	return nil
}
