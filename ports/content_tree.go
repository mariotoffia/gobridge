package ports

// WithoutEmptyCollections reduces a decoded document tree — the map[string]any,
// []any and scalar values a JSON decoder produces into an `any` — so that an
// absent collection and an empty one are the same value in an object field,
// and reports whether anything is left to record. It is the second half of the
// content identity (ADR 0016), next to ContentNormalForm: the normal form
// shapes the blueprint, this shapes the projection of it that a
// content-identity check hashes or compares, including the decoded options of
// every plugin. Every current projection applies it so no two checks can
// disagree on it.
//
// It exists because marshalling and re-parsing does not preserve the difference
// between an absent collection and an empty one in an object field — a nil
// slice is written out as `[]` and comes back non-nil — so a projection that
// distinguished the two would give the same configuration two identities, one
// in memory and one read back from a document. Collapsing them is not a
// loosening: in this config model an empty collection and an absent one both
// mean "none", and nothing else is collapsed — an empty string, a zero and a
// false are values and stay.
//
// Object keys whose value carries nothing are dropped, which is what makes an
// absent key and an empty one identical. Inside an array nothing is dropped
// or replaced: an element's position and kind are content, and the wire format
// keeps an empty object, an empty list and null apart on a round trip, so they
// stay three different elements. An element's own fields still follow the
// object rule, and an element left with no fields stays an empty object.
//
// Two boundaries of the rule, both deliberate:
//
//   - A decoded tree does not say which objects came from struct fields and
//     which are data a plugin passes through verbatim (an AMQP argument table
//     held in a map[string]any, for example), so the object rule reaches into
//     both. An entry in such a data map whose value is an empty table or an
//     explicit null therefore compares like an absent entry. That fold cannot
//     be narrowed to struct fields: a nil slice or map field marshals as null
//     and reads back from a document as an empty collection, and the rollout
//     quorum depends on those two projecting alike. No shipped transport gives
//     such an entry a meaning of its own; if one ever does, that plugin's
//     options need a value that is neither null nor an empty collection to
//     carry it.
//   - Scalars pass through untouched, so the caller decides how numbers are
//     represented. Every current projection in this project decodes with
//     json.Decoder.UseNumber so an integer is never rounded through float64;
//     the one exception is the bridge's reader of digests recorded by releases
//     before the normal form, which reproduces their float64 rounding and
//     their older list rule on purpose.
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
			out = append(out, reducedElement(item))
		}
		return out, true
	default:
		return typed, true
	}
}

// reducedElement applies the object rule inside an array element without ever
// turning the element itself into something else: an object keeps its
// non-empty fields (or stays an empty object), a list keeps its elements (or
// stays an empty list), and a scalar or null is returned as it is.
func reducedElement(item any) any {
	switch typed := item.(type) {
	case map[string]any:
		if reduced, keep := WithoutEmptyCollections(typed); keep {
			return reduced
		}
		return map[string]any{}
	case []any:
		if reduced, keep := WithoutEmptyCollections(typed); keep {
			return reduced
		}
		return []any{}
	default:
		return item
	}
}
