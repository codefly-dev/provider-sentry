package provider

import "testing"

func TestClassifySentryError(t *testing.T) {
	cases := []struct {
		name string
		in   SentryError
		want string
	}{
		{"auth", SentryError{StatusCode: 401}, DiagAuthentication},
		{"permission", SentryError{StatusCode: 403, Detail: "You do not have permission"}, DiagPermission},
		{"scope", SentryError{StatusCode: 200, Detail: "insufficient scope"}, DiagPermission},
		{"rate limit status", SentryError{StatusCode: 429}, DiagRateLimit},
		{"rate limit header", SentryError{StatusCode: 200, RetryAfter: true}, DiagRateLimit},
		{"rate limit over permission", SentryError{StatusCode: 403, RetryAfter: true}, DiagRateLimit},
		{"not found", SentryError{StatusCode: 404}, DiagNotFound},
		{"server", SentryError{StatusCode: 503}, DiagOutcomeUnknown},
		{"validation", SentryError{StatusCode: 400}, DiagValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifySentryError(tc.in); got != tc.want {
				t.Fatalf("ClassifySentryError(%+v) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestRateLimitBounded(t *testing.T) {
	if !(RateLimit{Remaining: 5}).Bounded() {
		t.Fatal("remaining budget must be bounded")
	}
	if (RateLimit{Remaining: 0}).Bounded() {
		t.Fatal("no remaining budget must not be bounded; observation must back off")
	}
}
