package shared

import (
	"errors"
	"net/url"
	"strings"
)

// RedactURI removes credential-bearing components from a URI so it can be
// safely written to a log line or wrapped into an error message. It strips
// userinfo (an embedded `user:pass@`), the query string, and the fragment —
// any of which may carry a secret — while preserving the scheme, host, and
// path needed to identify WHICH resource failed.
//
// It is best-effort and never panics: a value url.Parse rejects is redacted
// lexically (everything between the first `//` and the next `@` is dropped),
// so a malformed URI can never echo an embedded credential verbatim.
//
// Rationale: credential URIs (file://, pms://, vault://) are routinely
// interpolated into "invalid URI %q" errors and Warn logs. A URI of the form
// pms://user:s3cr3t@ns/param would otherwise leak the secret into an operator
// log the moment parsing failed.
func RedactURI(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return lexicalRedactURI(raw)
	}
	if u.User == nil && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery {
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return u.String()
}

// lexicalRedactURI drops userinfo, query, and fragment from a string url.Parse
// could not handle. The userinfo strip targets the `//authority` form only, so
// a bare `a:b` opaque URI is left intact.
func lexicalRedactURI(raw string) string {
	// Trim query/fragment first; a secret may hide in either.
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	schemeSep := strings.Index(raw, "//")
	if schemeSep < 0 {
		return raw
	}
	authStart := schemeSep + 2
	// Authority runs to the next '/'; userinfo is everything before an '@'
	// inside it.
	authEnd := len(raw)
	if slash := strings.IndexByte(raw[authStart:], '/'); slash >= 0 {
		authEnd = authStart + slash
	}
	if at := strings.LastIndexByte(raw[authStart:authEnd], '@'); at >= 0 {
		raw = raw[:authStart] + raw[authStart+at+1:]
	}
	return raw
}

// RedactURIError returns an error whose message can never contain a raw,
// credential-bearing URI. url.Parse and url.URL method failures wrap the
// offending URL in a *url.Error whose Error() prints it verbatim; this extracts
// that *url.Error from anywhere in the chain and returns a copy with the URL
// replaced by its RedactURI form (operation and underlying cause preserved).
//
// Any wrapper AROUND the *url.Error is intentionally discarded: a wrapper built
// with fmt.Errorf bakes the raw URL into its OWN message at construction time,
// so it cannot be un-redacted after the fact — mutating the inner error in place
// would leave the baked outer string leaking. Returning the freshly-redacted
// inner error is the only way to guarantee no raw URL survives. Call sites that
// need to keep surrounding context therefore redact at the SOURCE, before
// wrapping (see resolveRepo). Non-url.Error values pass through unchanged.
func RedactURIError(err error) error {
	if err == nil {
		return nil
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return &url.Error{Op: ue.Op, URL: RedactURI(ue.URL), Err: ue.Err}
	}
	return err
}
