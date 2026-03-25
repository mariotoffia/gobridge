package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		// In production, validate origin against allowed list
		return true
	},
}

// WebSocketMessage is the format for messages sent over WebSocket.
type WebSocketMessage struct {
	Type      string      `json:"type"`
	Timestamp time.Time   `json:"timestamp"`
	Data      interface{} `json:"data"`
}

// wsConnection wraps a WebSocket connection with synchronization.
type wsConnection struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	closed bool
}

func newWSConnection(conn *websocket.Conn) *wsConnection {
	return &wsConnection{conn: conn}
}

func (c *wsConnection) WriteJSON(v interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return websocket.ErrCloseSent
	}
	return c.conn.WriteJSON(v)
}

func (c *wsConnection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return c.conn.Close()
}

// ============================================================================
// Metrics Streaming
// ============================================================================

func (s *Server) handleStreamMetrics(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error("websocket upgrade failed", "error", err)
		return
	}

	ws := newWSConnection(conn)
	defer ws.Close()

	s.logger.Debug("metrics websocket connected", "remote", r.RemoteAddr)

	// Create a ticker for periodic updates
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// Handle incoming messages (for ping/pong and close)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	}()

	ctx := r.Context()

	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			metrics := s.bridge.Metrics()
			msg := WebSocketMessage{
				Type:      "metrics",
				Timestamp: time.Now().UTC(),
				Data:      metrics,
			}
			if err := ws.WriteJSON(msg); err != nil {
				s.logger.Debug("metrics websocket write failed", "error", err)
				return
			}
		}
	}
}

// ============================================================================
// Logs Streaming
// ============================================================================

// LogBuffer holds recent log entries for streaming.
type LogBuffer struct {
	mu      sync.RWMutex
	entries []LogEntry
	maxSize int
	subs    map[chan LogEntry]struct{}
	subsMu  sync.RWMutex
}

// LogEntry represents a log entry.
type LogEntry struct {
	Timestamp time.Time              `json:"timestamp"`
	Level     string                 `json:"level"`
	Message   string                 `json:"message"`
	Component string                 `json:"component,omitempty"`
	TraceID   string                 `json:"traceId,omitempty"`
	Fields    map[string]interface{} `json:"fields,omitempty"`
}

// NewLogBuffer creates a new log buffer.
func NewLogBuffer(maxSize int) *LogBuffer {
	return &LogBuffer{
		entries: make([]LogEntry, 0, maxSize),
		maxSize: maxSize,
		subs:    make(map[chan LogEntry]struct{}),
	}
}

// Add adds a log entry to the buffer and notifies subscribers.
func (b *LogBuffer) Add(entry LogEntry) {
	b.mu.Lock()
	if len(b.entries) >= b.maxSize {
		// Remove oldest
		b.entries = b.entries[1:]
	}
	b.entries = append(b.entries, entry)
	b.mu.Unlock()

	// Notify subscribers
	b.subsMu.RLock()
	for ch := range b.subs {
		select {
		case ch <- entry:
		default:
			// Drop if subscriber is slow
		}
	}
	b.subsMu.RUnlock()
}

// Subscribe returns a channel that receives new log entries.
func (b *LogBuffer) Subscribe() chan LogEntry {
	ch := make(chan LogEntry, 100)
	b.subsMu.Lock()
	b.subs[ch] = struct{}{}
	b.subsMu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber.
func (b *LogBuffer) Unsubscribe(ch chan LogEntry) {
	b.subsMu.Lock()
	delete(b.subs, ch)
	b.subsMu.Unlock()
	close(ch)
}

// logBuffer is the shared log buffer for streaming
var logBuffer = NewLogBuffer(1000)

// AddLogEntry adds a log entry to the shared buffer (call from logger).
func AddLogEntry(entry LogEntry) {
	logBuffer.Add(entry)
}

func (s *Server) handleStreamLogs(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error("websocket upgrade failed", "error", err)
		return
	}

	ws := newWSConnection(conn)
	defer ws.Close()

	s.logger.Debug("logs websocket connected", "remote", r.RemoteAddr)

	// Parse query parameters
	levelFilter := r.URL.Query().Get("level")
	componentFilter := r.URL.Query().Get("component")

	// Subscribe to log entries
	logCh := logBuffer.Subscribe()
	defer logBuffer.Unsubscribe(logCh)

	// Handle incoming messages
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	}()

	ctx := r.Context()

	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case entry := <-logCh:
			// Apply filters
			if levelFilter != "" && entry.Level != levelFilter {
				continue
			}
			if componentFilter != "" && entry.Component != componentFilter {
				continue
			}

			msg := WebSocketMessage{
				Type:      "log",
				Timestamp: entry.Timestamp,
				Data:      entry,
			}
			if err := ws.WriteJSON(msg); err != nil {
				s.logger.Debug("logs websocket write failed", "error", err)
				return
			}
		}
	}
}

// ============================================================================
// Traces Streaming
// ============================================================================

// TraceBuffer holds recent traces for streaming.
type TraceBuffer struct {
	mu      sync.RWMutex
	traces  []TraceSummary
	maxSize int
	subs    map[chan TraceSummary]struct{}
	subsMu  sync.RWMutex
}

// TraceSummary represents a trace summary for streaming.
type TraceSummary struct {
	TraceID       string    `json:"traceId"`
	RootSpan      string    `json:"rootSpan"`
	ServiceName   string    `json:"serviceName"`
	OperationName string    `json:"operationName"`
	StartTime     time.Time `json:"startTime"`
	Duration      string    `json:"duration"`
	SpanCount     int       `json:"spanCount"`
	Status        string    `json:"status"`
	HasErrors     bool      `json:"hasErrors"`
}

// NewTraceBuffer creates a new trace buffer.
func NewTraceBuffer(maxSize int) *TraceBuffer {
	return &TraceBuffer{
		traces:  make([]TraceSummary, 0, maxSize),
		maxSize: maxSize,
		subs:    make(map[chan TraceSummary]struct{}),
	}
}

// Add adds a trace to the buffer and notifies subscribers.
func (b *TraceBuffer) Add(trace TraceSummary) {
	b.mu.Lock()
	if len(b.traces) >= b.maxSize {
		b.traces = b.traces[1:]
	}
	b.traces = append(b.traces, trace)
	b.mu.Unlock()

	b.subsMu.RLock()
	for ch := range b.subs {
		select {
		case ch <- trace:
		default:
		}
	}
	b.subsMu.RUnlock()
}

// Subscribe returns a channel that receives new traces.
func (b *TraceBuffer) Subscribe() chan TraceSummary {
	ch := make(chan TraceSummary, 100)
	b.subsMu.Lock()
	b.subs[ch] = struct{}{}
	b.subsMu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber.
func (b *TraceBuffer) Unsubscribe(ch chan TraceSummary) {
	b.subsMu.Lock()
	delete(b.subs, ch)
	b.subsMu.Unlock()
	close(ch)
}

// traceBuffer is the shared trace buffer for streaming
var traceBuffer = NewTraceBuffer(500)

// AddTrace adds a trace to the shared buffer (call from tracer).
func AddTrace(trace TraceSummary) {
	traceBuffer.Add(trace)
}

func (s *Server) handleStreamTraces(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error("websocket upgrade failed", "error", err)
		return
	}

	ws := newWSConnection(conn)
	defer ws.Close()

	s.logger.Debug("traces websocket connected", "remote", r.RemoteAddr)

	// Subscribe to traces
	traceCh := traceBuffer.Subscribe()
	defer traceBuffer.Unsubscribe(traceCh)

	// Handle incoming messages
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	}()

	ctx := r.Context()

	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case trace := <-traceCh:
			msg := WebSocketMessage{
				Type:      "trace",
				Timestamp: trace.StartTime,
				Data:      trace,
			}
			if err := ws.WriteJSON(msg); err != nil {
				s.logger.Debug("traces websocket write failed", "error", err)
				return
			}
		}
	}
}

// ============================================================================
// WebSocket Hub for managing connections
// ============================================================================

// Hub maintains active WebSocket connections and broadcasts messages.
type Hub struct {
	// Registered clients by stream type
	clients   map[string]map[*wsConnection]bool
	clientsMu sync.RWMutex

	// Register requests
	register chan *clientRegistration

	// Unregister requests
	unregister chan *clientRegistration

	// Shutdown channel
	done chan struct{}
}

type clientRegistration struct {
	streamType string
	conn       *wsConnection
}

// NewHub creates a new Hub.
func NewHub() *Hub {
	return &Hub{
		clients:    make(map[string]map[*wsConnection]bool),
		register:   make(chan *clientRegistration),
		unregister: make(chan *clientRegistration),
		done:       make(chan struct{}),
	}
}

// Run starts the hub's main loop.
func (h *Hub) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.done:
			return
		case reg := <-h.register:
			h.clientsMu.Lock()
			if h.clients[reg.streamType] == nil {
				h.clients[reg.streamType] = make(map[*wsConnection]bool)
			}
			h.clients[reg.streamType][reg.conn] = true
			h.clientsMu.Unlock()

		case reg := <-h.unregister:
			h.clientsMu.Lock()
			if clients, ok := h.clients[reg.streamType]; ok {
				delete(clients, reg.conn)
			}
			h.clientsMu.Unlock()
		}
	}
}

// Broadcast sends a message to all clients of a stream type.
func (h *Hub) Broadcast(streamType string, msg interface{}) {
	h.clientsMu.RLock()
	clients := h.clients[streamType]
	h.clientsMu.RUnlock()

	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	for conn := range clients {
		conn.mu.Lock()
		if !conn.closed {
			conn.conn.WriteMessage(websocket.TextMessage, data)
		}
		conn.mu.Unlock()
	}
}

// Close shuts down the hub.
func (h *Hub) Close() {
	close(h.done)
}
