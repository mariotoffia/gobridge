package httpapi

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// certReloader serves a TLS server certificate through tls.Config.GetCertificate
// with an mtime-checked lazy reload. The initial load happens in newCertReloader
// so startup still fails fast on a bad or unreadable pair; thereafter a
// cert-manager renewal (which atomically replaces the files and so bumps their
// modification time) is picked up on the next handshake WITHOUT a process
// restart. A reload that fails mid-rotation (e.g. a half-written pair) keeps the
// last-good certificate rather than breaking TLS, and records the observed
// mtimes so it does not re-attempt (and re-log) on every subsequent handshake
// until the files change again.
type certReloader struct {
	certFile string
	keyFile  string
	logger   *slog.Logger

	mu      sync.RWMutex
	cert    *tls.Certificate
	certMod time.Time
	keyMod  time.Time
}

// newCertReloader loads the cert/key pair once (fail-fast) and returns a
// reloader whose getCertificate method backs tls.Config.GetCertificate.
func newCertReloader(certFile, keyFile string, logger *slog.Logger) (*certReloader, error) {
	cr := &certReloader{certFile: certFile, keyFile: keyFile, logger: logger}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS key pair: %w", err)
	}
	cr.cert = &cert
	cr.certMod, cr.keyMod = fileModTime(certFile), fileModTime(keyFile)
	return cr, nil
}

// getCertificate returns the current server certificate, reloading it first when
// either backing file's modification time has changed since the last load. It
// satisfies the tls.Config.GetCertificate signature. It never returns a nil
// certificate once construction succeeded: a failed reload falls back to the
// last-good pair.
func (cr *certReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	certMod, keyMod := fileModTime(cr.certFile), fileModTime(cr.keyFile)

	cr.mu.RLock()
	unchanged := certMod.Equal(cr.certMod) && keyMod.Equal(cr.keyMod)
	cert := cr.cert
	cr.mu.RUnlock()
	if unchanged {
		return cert, nil
	}

	cr.mu.Lock()
	defer cr.mu.Unlock()
	// Re-check under the write lock: a concurrent handshake may have reloaded.
	if certMod.Equal(cr.certMod) && keyMod.Equal(cr.keyMod) {
		return cr.cert, nil
	}
	next, err := tls.LoadX509KeyPair(cr.certFile, cr.keyFile)
	if err != nil {
		// Keep serving the last-good pair, but record the observed mtimes so we
		// do not re-attempt (and re-log) on every handshake until the files
		// change again — the common cause is reading a pair mid-replacement.
		cr.certMod, cr.keyMod = certMod, keyMod
		if cr.logger != nil {
			cr.logger.Warn("httpapi: TLS certificate reload failed; serving previous certificate",
				"cert_file", cr.certFile, "error", err)
		}
		return cr.cert, nil
	}
	cr.cert = &next
	cr.certMod, cr.keyMod = certMod, keyMod
	if cr.logger != nil {
		cr.logger.Info("httpapi: TLS certificate reloaded", "cert_file", cr.certFile)
	}
	return cr.cert, nil
}

// fileModTime returns the file's modification time, or the zero time when it
// cannot be stat'd. A zero time compares unequal to a real mtime, so a
// transiently-missing file (e.g. mid-rename) triggers a reload attempt that
// falls back to the last-good pair rather than being mistaken for "unchanged".
func fileModTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}
