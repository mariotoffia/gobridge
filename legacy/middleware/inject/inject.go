// Package inject provides a middleware for injecting test messages into pipelines.
//
// This middleware allows messages to be injected directly into a pipeline,
// bypassing the normal source. This is useful for:
//   - Testing pipeline processing without setting up external brokers
//   - Admin API message injection for troubleshooting
//   - Integration testing of middleware chains
//
// # Usage
//
// Add the middleware to a pipeline:
//
//	injector := inject.NewMiddleware()
//	pipeline := core.NewPipeline("test",
//	    source,
//	    target,
//	    injector, // First middleware to receive injected messages
//	    transform.NewMiddleware(...),
//	)
//
// Inject a message:
//
//	result, err := injector.Inject(ctx, &types.Message{
//	    Topic:   "test/topic",
//	    Payload: []byte(`{"test": true}`),
//	})
package inject

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// Middleware allows messages to be injected directly into a pipeline.
// It implements types.Middleware and can be added to any pipeline's middleware chain.
type Middleware struct {
	name string

	// pending holds messages waiting to be injected
	pending chan *injectionRequest
	// results maps request IDs to result channels
	results sync.Map

	// nextID generates unique request IDs
	nextID atomic.Uint64

	// timeout for waiting on results
	timeout time.Duration

	// closed indicates the middleware is shut down
	closed atomic.Bool
}

// injectionRequest holds a message to be injected and its result channel.
type injectionRequest struct {
	id       uint64
	msg      *types.Message
	resultCh chan *InjectionResult
}

// InjectionResult contains the result of an injection.
type InjectionResult struct {
	// MessageID is the ID of the injected message (generated)
	MessageID string `json:"messageId"`

	// Topic is the topic the message was injected to
	Topic string `json:"topic"`

	// Success indicates whether the message was processed successfully
	Success bool `json:"success"`

	// Error contains any error that occurred
	Error error `json:"error,omitempty"`

	// ProcessingTime is how long it took to process the message
	ProcessingTime time.Duration `json:"processingTime"`

	// Metadata contains any metadata added during processing
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// Option configures the middleware.
type Option func(*Middleware)

// WithName sets the middleware name.
func WithName(name string) Option {
	return func(m *Middleware) {
		m.name = name
	}
}

// WithBufferSize sets the pending message buffer size.
func WithBufferSize(size int) Option {
	return func(m *Middleware) {
		m.pending = make(chan *injectionRequest, size)
	}
}

// WithTimeout sets the result timeout.
func WithTimeout(timeout time.Duration) Option {
	return func(m *Middleware) {
		m.timeout = timeout
	}
}

// NewMiddleware creates a new inject middleware.
func NewMiddleware(opts ...Option) *Middleware {
	m := &Middleware{
		name:    "inject",
		pending: make(chan *injectionRequest, 100),
		timeout: 30 * time.Second,
	}

	for _, opt := range opts {
		opt(m)
	}

	return m
}

// Name returns the middleware name.
func (m *Middleware) Name() string {
	return m.name
}

// Process implements types.Middleware.
// It processes both injected messages and regular source messages.
func (m *Middleware) Process(ctx context.Context, msg *types.Message, next types.MiddlewareFunc) error {
	// First, check for any pending injected messages
	select {
	case req := <-m.pending:
		// Process the injected message
		return m.processInjected(ctx, req, next)
	default:
		// No pending injections, process the regular message
		return next(ctx, msg)
	}
}

// processInjected handles an injected message request.
func (m *Middleware) processInjected(ctx context.Context, req *injectionRequest, next types.MiddlewareFunc) error {
	start := time.Now()
	result := &InjectionResult{
		MessageID: fmt.Sprintf("inject-%d", req.id),
		Topic:     req.msg.Topic,
		Metadata:  make(map[string]interface{}),
	}

	// Process through the rest of the middleware chain
	err := next(ctx, req.msg)

	result.ProcessingTime = time.Since(start)
	result.Success = err == nil
	result.Error = err

	// Send result back
	select {
	case req.resultCh <- result:
	default:
		// Result channel full or closed, log and continue
	}

	return err
}

// Inject injects a message into the pipeline and waits for the result.
// This method blocks until the message is processed or the timeout expires.
func (m *Middleware) Inject(ctx context.Context, msg *types.Message) (*InjectionResult, error) {
	if m.closed.Load() {
		return nil, fmt.Errorf("middleware is closed")
	}

	// Ensure timestamp
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}

	// Create request
	req := &injectionRequest{
		id:       m.nextID.Add(1),
		msg:      msg,
		resultCh: make(chan *InjectionResult, 1),
	}

	// Send to pending queue
	select {
	case m.pending <- req:
		// Request queued
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(m.timeout):
		return nil, fmt.Errorf("timeout waiting to queue injection")
	}

	// Wait for result
	select {
	case result := <-req.resultCh:
		return result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(m.timeout):
		return nil, fmt.Errorf("timeout waiting for injection result")
	}
}

// InjectAsync injects a message without waiting for the result.
// Returns a channel that will receive the result when available.
func (m *Middleware) InjectAsync(ctx context.Context, msg *types.Message) (<-chan *InjectionResult, error) {
	if m.closed.Load() {
		return nil, fmt.Errorf("middleware is closed")
	}

	// Ensure timestamp
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}

	// Create request
	req := &injectionRequest{
		id:       m.nextID.Add(1),
		msg:      msg,
		resultCh: make(chan *InjectionResult, 1),
	}

	// Send to pending queue
	select {
	case m.pending <- req:
		return req.resultCh, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, fmt.Errorf("injection queue full")
	}
}

// InjectWithTopic creates a message with the given topic and payload and injects it.
func (m *Middleware) InjectWithTopic(ctx context.Context, topic string, payload []byte, metadata map[string]interface{}) (*InjectionResult, error) {
	msg := &types.Message{
		Topic:    topic,
		Payload:  payload,
		Metadata: metadata,
	}
	return m.Inject(ctx, msg)
}

// PendingCount returns the number of pending injection requests.
func (m *Middleware) PendingCount() int {
	return len(m.pending)
}

// Close closes the middleware and drains pending requests.
func (m *Middleware) Close() error {
	if m.closed.Swap(true) {
		return nil // Already closed
	}

	// Drain pending requests with error
	close(m.pending)
	for req := range m.pending {
		result := &InjectionResult{
			MessageID: fmt.Sprintf("inject-%d", req.id),
			Topic:     req.msg.Topic,
			Success:   false,
			Error:     fmt.Errorf("middleware closed"),
		}
		select {
		case req.resultCh <- result:
		default:
		}
	}

	return nil
}

// Trigger processes any pending injected messages.
// This should be called by the pipeline to process injections when no source messages are available.
// Returns true if an injection was processed.
func (m *Middleware) Trigger(ctx context.Context, next types.MiddlewareFunc) (bool, error) {
	select {
	case req := <-m.pending:
		if req == nil {
			return false, nil
		}
		return true, m.processInjected(ctx, req, next)
	default:
		return false, nil
	}
}

// Ensure Middleware implements types.Middleware at compile time.
var _ types.Middleware = (*Middleware)(nil)
