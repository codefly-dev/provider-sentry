package provider

import "testing"

func TestParseOriginClassifiesRegions(t *testing.T) {
	cases := map[string]originRegion{
		"":                              regionDefault, // empty defaults to the SaaS default
		"https://sentry.io":             regionDefault,
		"https://us.sentry.io":          regionUS,
		"https://de.sentry.io":          regionDE,
		"https://sentry.acme.internal":  regionSelfHosted,
		"http://sentry.io":              regionInvalid, // non-https
		"https://sentry.io/some/path":   regionInvalid, // an origin carries no path
		"not a url at all":              regionInvalid,
	}
	for raw, want := range cases {
		t.Run(raw, func(t *testing.T) {
			if got := parseOrigin(raw).Region; got != want {
				t.Fatalf("parseOrigin(%q).Region = %q, want %q", raw, got, want)
			}
		})
	}
}

func TestSaaSOriginsAreWithinManifestCeiling(t *testing.T) {
	// Every SaaS origin the provider recognizes must be a host the manifest's
	// bounded origin rule actually admits.
	m := testServer(t).manifest
	rule := m.OriginRules[0]
	for host := range saasRegions {
		matched := false
		for _, pattern := range rule.HostPatterns {
			if host == pattern || (len(pattern) > 2 && pattern[0] == '*' && len(host) > len(pattern)-1 && host[len(host)-len(pattern)+1:] == pattern[1:]) {
				matched = true
			}
		}
		if !matched {
			t.Fatalf("SaaS host %q is not within the manifest origin ceiling %v", host, rule.HostPatterns)
		}
	}
}
