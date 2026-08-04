package provider

import (
	"net/url"
	"strings"
)

// originRegion classifies an admitted Sentry API origin. The three SaaS silos
// share one bounded origin rule in the manifest; anything else is a self-hosted
// destination that the host must admit explicitly per deployment.
type originRegion string

const (
	regionDefault    originRegion = "default"     // sentry.io
	regionUS         originRegion = "us"          // us.sentry.io
	regionDE         originRegion = "de"          // de.sentry.io
	regionSelfHosted originRegion = "self-hosted" // an explicitly admitted private/self-hosted origin
	regionInvalid    originRegion = "invalid"     // not a usable https origin
)

// defaultOrigin is the Sentry SaaS default when a binding supplies no origin.
const defaultOrigin = "https://sentry.io"

// saasRegions maps the admitted SaaS hosts to their region. These are the exact
// hosts the manifest `api` origin rule covers.
var saasRegions = map[string]originRegion{
	"sentry.io":    regionDefault,
	"us.sentry.io": regionUS,
	"de.sentry.io": regionDE,
}

// origin is a normalized, non-secret API origin selection. It is bound into the
// plan so the admitted scheme/host/port is visible and attributable.
type origin struct {
	Raw    string
	Scheme string
	Host   string
	Port   string
	Region originRegion
}

// parseOrigin normalizes and classifies a binding-supplied origin. An empty
// origin defaults to the Sentry SaaS default. A syntactically unusable origin is
// classified regionInvalid and rejected by validation; a well-formed non-SaaS
// origin is classified regionSelfHosted and requires explicit host admission.
func parseOrigin(raw string) origin {
	if raw == "" {
		raw = defaultOrigin
	}
	raw = strings.TrimRight(raw, "/")
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return origin{Raw: raw, Region: regionInvalid}
	}
	host := strings.ToLower(parsed.Hostname())
	o := origin{Raw: raw, Scheme: parsed.Scheme, Host: host, Port: parsed.Port()}
	switch {
	case parsed.Scheme != "https":
		o.Region = regionInvalid
	case host == "":
		o.Region = regionInvalid
	default:
		if region, ok := saasRegions[host]; ok {
			o.Region = region
		} else {
			o.Region = regionSelfHosted
		}
	}
	return o
}
