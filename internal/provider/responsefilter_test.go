package provider

import (
	"bytes"
	"context"
	"testing"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/manifest"
	"github.com/codefly-dev/core/provider/responsepolicy"
)

// nopSink is a capture sink that fails if it is ever asked to store a secret.
// The Sentry manifest suppresses (never captures) the legacy secret fields, so
// no capture must ever occur.
type nopSink struct{ t *testing.T }

func (s nopSink) Put(context.Context, responsepolicy.SinkTarget, string) (*providerv0.OpaqueReference, error) {
	s.t.Fatal("no client-key secret must ever be captured; the manifest suppresses them")
	return nil, nil
}

// clientKeyPolicy compiles the packaged manifest's client_key_list response
// schema into a host response policy. required marks the load-bearing forward
// (dsn.public) and both legacy secret suppressions as required, so a moved or
// renamed field fails closed.
func clientKeyPolicy(t *testing.T, required bool) responsepolicy.Policy {
	t.Helper()
	m, err := manifest.Load(loadManifestBytes(t))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var fields []responsepolicy.Field
	for _, schema := range m.ResponseSchemas {
		if schema.ID != "client_key_list" {
			continue
		}
		for _, f := range schema.Fields {
			field := responsepolicy.Field{Selector: f.Selector, Disposition: f.Disposition}
			if required {
				field.Required = true
			}
			fields = append(fields, field)
		}
	}
	if len(fields) == 0 {
		t.Fatal("client_key_list response schema not found in manifest")
	}
	return responsepolicy.Policy{Fields: fields, Limits: responsepolicy.DefaultLimits()}
}

const (
	poisonSecret    = "srt_legacy_secret_value"
	poisonDSNSecret = "https://pub:srt_dsn_secret_value@sentry.io/1"
)

// poisonBody is a Sentry client-key list (a bare array) carrying both legacy
// secret locations beside the public DSN.
const poisonBody = `[{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","name":"Default","isActive":true,` +
	`"public":"pub_1","secret":"srt_legacy_secret_value",` +
	`"rateLimit":{"window":60,"count":100},` +
	`"dsn":{"public":"https://pub_1@sentry.io/1","secret":"https://pub:srt_dsn_secret_value@sentry.io/1"}}]`

func TestManifestSuppressesLegacyClientKeySecrets(t *testing.T) {
	result, err := clientKeyPolicy(t, false).Filter(context.Background(), []byte(poisonBody), "", "application/json", nopSink{t})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}

	// Both legacy secret locations are suppressed to presence-only.
	if len(result.Suppressed) != 2 {
		t.Fatalf("expected both legacy secret locations suppressed, got %v", result.Suppressed)
	}
	if len(result.Captures) != 0 {
		t.Fatal("no secret must be captured; they are suppressed")
	}

	// The public DSN is forwarded; the secret bytes appear nowhere.
	forwardedDSN := false
	for _, f := range result.Forwarded {
		if f.Value.GetStringValue() == "https://pub_1@sentry.io/1" {
			forwardedDSN = true
		}
		assertAbsent(t, []byte(f.Value.GetStringValue()))
	}
	if !forwardedDSN {
		t.Fatal("the public DSN must be forwarded to provider code")
	}
	assertAbsent(t, result.SafeJSON)
}

func TestManifestFailsClosedOnMovedSecret(t *testing.T) {
	// The legacy dsn.secret was renamed away: a required suppression can no
	// longer confirm it was handled, so the filter must fail closed.
	const moved = `[{"id":"a","public":"pub_1","secret":"srt_legacy_secret_value",` +
		`"dsn":{"public":"https://pub_1@sentry.io/1","sekret":"https://pub:leak@sentry.io/1"}}]`
	_, err := clientKeyPolicy(t, true).Filter(context.Background(), []byte(moved), "", "application/json", nopSink{t})
	if err == nil {
		t.Fatal("a moved/renamed required secret field must fail closed")
	}
}

func assertAbsent(t *testing.T, haystack []byte) {
	t.Helper()
	for _, needle := range []string{poisonSecret, poisonDSNSecret, "srt_dsn_secret_value"} {
		if bytes.Contains(haystack, []byte(needle)) {
			t.Fatalf("poison secret leaked into filtered output: %q", string(haystack))
		}
	}
}
