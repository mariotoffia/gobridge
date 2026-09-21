package ports

// WithoutEmptyCollections reduces a decoded document tree — the map[string]any,
// []any and scalar values a JSON decoder produces into an `any` — so that an
// absent collection and an empty one are the same value, and reports whether
// anything is left to record. It is the second half of the content identity
// (ADR 0016), next to ContentNormalForm: the normal form shapes the blueprint,
// this shapes the projection of it that a content-identity check hashes or
// compares, including the decoded options of every plugin. Every projection
// applies it so no two checks can disagree on it.
//
// It exists because marshalling and re-parsing does not preserve the difference
// between an absent collection and an empty one — a nil slice is written out as
// `[]` and comes back non-nil — so a projection that distinguished the two would
// give the same configuration two identities, one in memory and one read back
// from a document. Collapsing them is not a loosening: in this config model an
// empty collection and an absent one both mean "none", and nothing else is
// collapsed — an empty string, a zero and a false are values and stay.
//
// Object keys whose value carries nothing are dropped, which is what makes an
// absent key and an empty one identical. Array ELEMENTS are never dropped —
// position is meaning in an array — so an element that carries nothing stays as
// a nil placeholder and a shorter array still differs from a longer one.
//
// Two boundaries of the rule, both deliberate:
//
//   - A decoded tree does not say which objects came from struct fields and
//     which are data a plugin passes through verbatim (an AMQP argument table
//     held in a map[string]any, for example), so the rule reaches into both.
//     An entry in such a data map whose value is an empty table therefore
//     compares like an absent entry. No shipped transport gives such an entry
//     a meaning of its own; if one ever does, that plugin's options need a
//     value that is not an empty collection to carry it.
//   - Scalars pass through untouched, so the caller decides how numbers are
//     represented. Every projection in this project decodes with
//     json.Decoder.UseNumber so an integer is never rounded through float64.
func WithoutEmptyCollections(value any) (any, bool) {
	switch typed := value.(type) {
	case nil:
		return nil, false
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if normalized, keep := WithoutEmptyCollections(item); keep {
				out[key] = normalized
			}
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	case []any:
		if len(typed) == 0 {
			return nil, false
		}
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			normalized, keep := WithoutEmptyCollections(item)
			if !keep {
				normalized = nil
			}
			out = append(out, normalized)
		}
		return out, true
	default:
		return typed, true
	}
}
