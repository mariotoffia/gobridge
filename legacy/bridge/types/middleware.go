package types

import "context"

// Middleware processes messages in a pipeline chain.
// Each middleware can inspect, modify, or filter messages before passing to the next handler.
type Middleware interface {
	// Name returns a unique identifier for this middleware (for logging/metrics).
	Name() string
	// Process handles a message and calls next to continue the chain.
	// Return an error to stop processing and propagate the error up the chain.
	// The middleware may modify the message before calling next.
	Process(ctx context.Context, msg *Message, next MiddlewareFunc) error
}

// MiddlewareFunc is the function signature for the next handler in the chain.
type MiddlewareFunc func(ctx context.Context, msg *Message) error

// MiddlewareAdapter wraps a function to implement the Middleware interface.
type MiddlewareAdapter struct {
	name string
	fn   func(ctx context.Context, msg *Message, next MiddlewareFunc) error
}

// NewMiddlewareAdapter creates a new MiddlewareAdapter with the given name and function.
func NewMiddlewareAdapter(name string, fn func(ctx context.Context, msg *Message, next MiddlewareFunc) error) *MiddlewareAdapter {
	return &MiddlewareAdapter{name: name, fn: fn}
}

func (m *MiddlewareAdapter) Name() string {
	return m.name
}

func (m *MiddlewareAdapter) Process(ctx context.Context, msg *Message, next MiddlewareFunc) error {
	return m.fn(ctx, msg, next)
}

// MiddlewareChain combines multiple middlewares into a single processing chain.
type MiddlewareChain struct {
	middlewares []Middleware
}

// NewMiddlewareChain creates a new chain from the given middlewares.
// Middlewares are executed in order (first added = first executed).
func NewMiddlewareChain(middlewares ...Middleware) *MiddlewareChain {
	return &MiddlewareChain{middlewares: middlewares}
}

// Append adds middlewares to the end of the chain.
func (c *MiddlewareChain) Append(middlewares ...Middleware) *MiddlewareChain {
	c.middlewares = append(c.middlewares, middlewares...)
	return c
}

// Prepend adds middlewares to the beginning of the chain.
func (c *MiddlewareChain) Prepend(middlewares ...Middleware) *MiddlewareChain {
	c.middlewares = append(middlewares, c.middlewares...)
	return c
}

// Process executes the middleware chain with the given message.
// The finalHandler is called after all middlewares have processed the message.
func (c *MiddlewareChain) Process(ctx context.Context, msg *Message, finalHandler MiddlewareFunc) error {
	if len(c.middlewares) == 0 {
		return finalHandler(ctx, msg)
	}

	// Build the chain from the end
	handler := finalHandler
	for i := len(c.middlewares) - 1; i >= 0; i-- {
		mw := c.middlewares[i]
		next := handler
		handler = func(ctx context.Context, msg *Message) error {
			return mw.Process(ctx, msg, next)
		}
	}

	return handler(ctx, msg)
}

// Len returns the number of middlewares in the chain.
func (c *MiddlewareChain) Len() int {
	return len(c.middlewares)
}

// Names returns the names of all middlewares in order.
func (c *MiddlewareChain) Names() []string {
	names := make([]string, len(c.middlewares))
	for i, mw := range c.middlewares {
		names[i] = mw.Name()
	}
	return names
}

