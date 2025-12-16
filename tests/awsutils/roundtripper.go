// Package awsutils provides test utilities for AWS service testing,
// including HTTP RoundTripper for error injection.
package awsutils

import (
	"bytes"
	"io"
	"net/http"
	"sync"
)

// ============================================================================
// Stack - Generic LIFO Stack
// ============================================================================

// Stack is a generic LIFO stack implementation.
type Stack[T any] []T

// Push adds an item to the top of the stack.
func (s *Stack[T]) Push(v T) {
	*s = append(*s, v)
}

// PushMany adds multiple items to the top of the stack.
func (s *Stack[T]) PushMany(v ...T) {
	*s = append(*s, v...)
}

// Pop removes and returns the top item from the stack.
// Panics if the stack is empty.
func (s *Stack[T]) Pop() T {
	n := len(*s)
	if n == 0 {
		var zero T
		return zero
	}
	v := (*s)[n-1]
	*s = (*s)[:n-1]
	return v
}

// Peek returns the top item without removing it.
// Returns zero value if empty.
func (s *Stack[T]) Peek() T {
	n := len(*s)
	if n == 0 {
		var zero T
		return zero
	}
	return (*s)[n-1]
}

// Clear removes all items from the stack.
func (s *Stack[T]) Clear() {
	*s = (*s)[:0]
}

// Len returns the number of items in the stack.
func (s Stack[T]) Len() int {
	return len(s)
}

// IsEmpty returns true if the stack is empty.
func (s Stack[T]) IsEmpty() bool {
	return len(s) == 0
}

// ============================================================================
// RoundTripperTransaction - HTTP Transaction Configuration
// ============================================================================

// RoundTripperTransaction represents a single HTTP transaction configuration.
//
// It can either be latched or put into a queue that the RoundTripper will
// use to determine whether to simulate a body and result code or pass through
// the request to the target server.
type RoundTripperTransaction struct {
	// Status is the HTTP status code to return.
	Status int
	// Body is the response body to return.
	Body string
	// Headers are optional response headers to include.
	Headers map[string]string
	// Error is an optional error to return instead of a response.
	// When set, Status and Body are ignored.
	Error error
}

// ============================================================================
// RoundTripper - HTTP Transport Interceptor
// ============================================================================

// RoundTripper is an http.RoundTripper implementation that can intercept
// HTTP requests and return configured responses for testing purposes.
//
// It supports two modes of operation:
//  1. Queue mode: Transactions are pushed onto a stack and popped for each request.
//  2. Latch mode: A single transaction is used for all requests until unlatched.
//
// When both modes are inactive (no transactions and not latched), requests
// pass through to the inner transport.
type RoundTripper struct {
	inner   http.RoundTripper
	mu      sync.RWMutex
	enabled bool
	// transactions is a stack of queued responses.
	// When empty and latched is nil, the RoundTripper will pass through
	// to the inner transport even if enabled.
	transactions Stack[RoundTripperTransaction]
	// latched will override the queue if set.
	latched *RoundTripperTransaction
}

// NewRoundTripper creates a new RoundTripper wrapping the given transport.
// If inner is nil, http.DefaultTransport is used.
func NewRoundTripper(inner http.RoundTripper) *RoundTripper {
	if inner == nil {
		inner = http.DefaultTransport
	}
	return &RoundTripper{
		inner:        inner,
		transactions: Stack[RoundTripperTransaction]{},
	}
}

// Latch sets a transaction to be returned for all requests.
// The latched transaction takes precedence over queued transactions.
func (r *RoundTripper) Latch(tx RoundTripperTransaction) *RoundTripper {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latched = &tx
	return r
}

// Unlatch removes the latched transaction.
func (r *RoundTripper) Unlatch() *RoundTripper {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latched = nil
	return r
}

// Push adds one or more transactions to the queue.
// Transactions are processed in LIFO order (last pushed = first returned).
func (r *RoundTripper) Push(tx ...RoundTripperTransaction) *RoundTripper {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transactions.PushMany(tx...)
	return r
}

// PushN adds the same transaction N times to the queue.
func (r *RoundTripper) PushN(tx RoundTripperTransaction, n int) *RoundTripper {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := 0; i < n; i++ {
		r.transactions.Push(tx)
	}
	return r
}

// Clear removes all queued transactions.
func (r *RoundTripper) Clear() *RoundTripper {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transactions.Clear()
	return r
}

// Enable activates the RoundTripper to intercept requests.
func (r *RoundTripper) Enable() *RoundTripper {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled = true
	return r
}

// Disable deactivates the RoundTripper, passing all requests to the inner transport.
func (r *RoundTripper) Disable() *RoundTripper {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled = false
	return r
}

// IsEnabled returns true if the RoundTripper is enabled.
func (r *RoundTripper) IsEnabled() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.enabled
}

// HasTransactions returns true if there are queued transactions.
func (r *RoundTripper) HasTransactions() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return !r.transactions.IsEmpty()
}

// IsLatched returns true if a transaction is latched.
func (r *RoundTripper) IsLatched() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.latched != nil
}

// RoundTrip implements http.RoundTripper.
func (r *RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.enabled {
		return r.inner.RoundTrip(req)
	}

	var tx *RoundTripperTransaction

	// Check for latched transaction first
	if r.latched != nil {
		tx = r.latched
	} else if !r.transactions.IsEmpty() {
		// Pop from queue
		popped := r.transactions.Pop()
		tx = &popped
	}

	// If no transaction available, pass through
	if tx == nil {
		return r.inner.RoundTrip(req)
	}

	// Return error if configured
	if tx.Error != nil {
		return nil, tx.Error
	}

	// Build response
	resp := &http.Response{
		StatusCode: tx.Status,
		Status:     http.StatusText(tx.Status),
		Body:       io.NopCloser(bytes.NewBufferString(tx.Body)),
		Header:     make(http.Header),
		Request:    req,
	}

	// Add configured headers
	for k, v := range tx.Headers {
		resp.Header.Set(k, v)
	}

	// Add content type if body is present
	if tx.Body != "" && resp.Header.Get("Content-Type") == "" {
		resp.Header.Set("Content-Type", "application/json")
	}

	return resp, nil
}

// Ensure RoundTripper implements http.RoundTripper
var _ http.RoundTripper = (*RoundTripper)(nil)
