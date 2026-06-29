// Package aggdata is aggcheck analysistest fixture data for the
// aggregate-root marker track: marked roots forbid exported mutable
// fields and unguarded pointer transitions; unmarked types are ignored.
package aggdata

import "errors"

// CleanRecord is a compliant aggregate root: explicitly marked, all
// state is private, and its sole mutator is guarded (returns an error).
//
// aggregate-root
type CleanRecord struct {
	id     string
	status int
}

// NewCleanRecord constructs a CleanRecord.
func NewCleanRecord(id string) *CleanRecord { return &CleanRecord{id: id} }

// ID exposes the immutable identity through a read-only accessor.
func (r *CleanRecord) ID() string { return r.id }

// Advance is a guarded pointer transition: it mutates state but returns
// an error so an invariant violation can be rejected.
func (r *CleanRecord) Advance(to int) error {
	if to < r.status {
		return errors.New("cannot move backwards")
	}
	r.status = to
	return nil
}

// LeakyFields is marked but exposes exported mutable fields, defeating
// encapsulation. Its mutator is guarded, so only the fields are flagged.
//
// aggregate-root
type LeakyFields struct {
	ID     string // want `aggregate root "LeakyFields" exposes exported mutable field "ID"`
	secret string
	Status int // want `aggregate root "LeakyFields" exposes exported mutable field "Status"`
}

// Touch is a guarded mutator (returns an error) so it is not flagged.
func (r *LeakyFields) Touch() error {
	r.secret = "x"
	return nil
}

// Unguarded is marked and fully private, but its mutator returns nothing
// — an unguarded pointer transition that cannot reject an invariant
// violation.
//
// aggregate-root
type Unguarded struct {
	status int
}

// SetStatus mutates state and returns nothing.
func (r *Unguarded) SetStatus(s int) { // want `aggregate root "Unguarded" has unguarded pointer transition "SetStatus"`
	r.status = s
}

// NotAnAggregate has exported mutable fields and a naked setter but is
// NOT marked, so the analyzer leaves it alone.
type NotAnAggregate struct {
	Name  string
	Count int
}

// SetName mutates without returning — allowed because the type is not a
// marked aggregate root.
func (n *NotAnAggregate) SetName(s string) { n.Name = s }

// AllowedMutators is a marked aggregate root whose void pointer-receiver
// mutators are explicitly exempted via //aggcheck:allow-unguarded. Those
// methods must NOT be flagged. The un-exempted mutator SetBad must still
// be flagged.
//
// aggregate-root
type AllowedMutators struct {
	label string
	count int
}

// SetLabel is an always-valid extension point: no failable invariant, so
// the allow marker is appropriate.
//
//aggcheck:allow-unguarded
func (a *AllowedMutators) SetLabel(s string) { a.label = s }

// SetCount is also exempt.
//
// aggcheck:allow-unguarded
func (a *AllowedMutators) SetCount(n int) { a.count = n }

// SetBad has NO allow marker, so it is still flagged.
func (a *AllowedMutators) SetBad(n int) { // want `aggregate root "AllowedMutators" has unguarded pointer transition "SetBad"`
	a.count = n
}

// IndexMutators verifies that mutation THROUGH a receiver field — a map/slice
// element or a pointer dereference, not just a direct field assignment — is
// detected as an unguarded transition.
//
// aggregate-root
type IndexMutators struct {
	items map[string]int
	buf   []byte
	ptr   *int
}

// PokeMap writes a map element of the receiver: unguarded, must be flagged.
func (m *IndexMutators) PokeMap(k string, v int) { // want `aggregate root "IndexMutators" has unguarded pointer transition "PokeMap"`
	m.items[k] = v
}

// PokeSlice writes a slice element of the receiver: unguarded, must be flagged.
func (m *IndexMutators) PokeSlice(i int, b byte) { // want `aggregate root "IndexMutators" has unguarded pointer transition "PokeSlice"`
	m.buf[i] = b
}

// PokePtr writes through a pointer field of the receiver: unguarded, must be flagged.
func (m *IndexMutators) PokePtr(v int) { // want `aggregate root "IndexMutators" has unguarded pointer transition "PokePtr"`
	*m.ptr = v
}

// PokeAllowed writes a map element but is explicitly exempt: must NOT be
// flagged (the real-world Envelope.SetHeader case).
//
//aggcheck:allow-unguarded
func (m *IndexMutators) PokeAllowed(k string, v int) { m.items[k] = v }
