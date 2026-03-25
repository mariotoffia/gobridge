// Package httpapi provides admin and monitor HTTP servers for the GoBridge
// runtime. The admin server exposes bridge lifecycle, route inspection,
// and DLQ management behind mandatory API key authentication. The monitor
// server exposes unauthenticated health/liveness/readiness probes for
// orchestrators, plus authenticated endpoints for topology, route detail,
// and log access. CORS is disabled by default and wildcard origins are
// rejected at startup.
package httpapi
