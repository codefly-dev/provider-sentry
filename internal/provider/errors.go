package provider

import "strings"

// SentryError is the bounded, safe classification of a Sentry API failure. It
// carries only the HTTP status and Sentry's own coarse error kind — never the
// raw error body, detail message, or any project/organization data.
type SentryError struct {
	StatusCode int
	Detail     string // Sentry error object "detail" kind (e.g. "permission_denied"); never free-form user data.
	RetryAfter bool   // A Retry-After / X-Sentry-Rate-Limit header was present.
}

// ClassifySentryError maps a Sentry failure onto a stable provider diagnostic
// code. The mapping is intentionally coarse and provider-neutral: it never
// surfaces the raw Sentry message, so no project or organization data can leak
// through a diagnostic.
func ClassifySentryError(e SentryError) string {
	detail := strings.ToLower(strings.TrimSpace(e.Detail))

	switch {
	// A rate-limit signal is transient and authoritative: an explicit
	// Retry-After / X-Sentry-Rate-Limit header (or a 429) means back off, even
	// when the status is a 403 that would otherwise read as a permission denial.
	case e.RetryAfter || e.StatusCode == 429 || strings.Contains(detail, "rate limit"):
		return DiagRateLimit
	case e.StatusCode == 401 || strings.Contains(detail, "authentication"):
		return DiagAuthentication
	case e.StatusCode == 403 || strings.Contains(detail, "permission") || strings.Contains(detail, "scope"):
		return DiagPermission
	case e.StatusCode == 404 || strings.Contains(detail, "not found"):
		return DiagNotFound
	case e.StatusCode >= 500:
		return DiagOutcomeUnknown
	default:
		return DiagValidation
	}
}

// RateLimit is the normalized, safe projection of Sentry's rate-limit response
// headers. It carries only bounded numeric windows, never a caller identity or
// token.
type RateLimit struct {
	Limit          int64 // documented per-window request ceiling
	Remaining      int64 // requests remaining in the current window
	ResetEpoch     int64 // unix time the window resets
	ConcurrentUsed int64 // concurrent requests in flight, where Sentry reports it
	Concurrent     int64 // concurrent-request ceiling, where Sentry reports it
}

// Bounded reports whether observation may proceed without aggressive polling: a
// caller with no remaining budget before reset must back off rather than spin.
func (r RateLimit) Bounded() bool {
	return r.Remaining > 0
}
