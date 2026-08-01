package provider

import (
	"testing"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
)

func validate(t *testing.T, input map[string]*providerv0.PublicValue) *providerv0.ValidateResponse {
	t.Helper()
	response, err := testServer(t).Validate(t.Context(), &providerv0.ValidateRequest{
		Context: &providerv0.OfflineProviderContext{Binding: binding(), Mode: providerv0.HostMode_HOST_MODE_DEVELOPMENT, Input: input},
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	return response
}

func TestValidateAcceptsWellFormedInput(t *testing.T) {
	if !validate(t, validInput()).GetValid() {
		t.Fatal("expected valid input to pass")
	}
}

func TestValidateRejectsMissingOrBadFields(t *testing.T) {
	cases := map[string]func(map[string]*providerv0.PublicValue){
		"missing org":     func(m map[string]*providerv0.PublicValue) { delete(m, "organization_slug") },
		"missing project": func(m map[string]*providerv0.PublicValue) { delete(m, "project_slug") },
		"bad org slug":    func(m map[string]*providerv0.PublicValue) { m["organization_slug"] = str("Not A Slug") },
		"invalid origin":  func(m map[string]*providerv0.PublicValue) { m["api_origin"] = str("ftp://sentry.io") },
		"http origin":     func(m map[string]*providerv0.PublicValue) { m["api_origin"] = str("http://sentry.io") },
		"bad key id":      func(m map[string]*providerv0.PublicValue) { m["client_key_id"] = str("not-hex") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			input := validInput()
			mutate(input)
			if validate(t, input).GetValid() {
				t.Fatalf("expected %q to be rejected", name)
			}
		})
	}
}

func TestValidateAcceptsRegionalOrigins(t *testing.T) {
	for _, host := range []string{"https://us.sentry.io", "https://de.sentry.io"} {
		input := validInput()
		input["api_origin"] = str(host)
		if !validate(t, input).GetValid() {
			t.Fatalf("regional origin %q must validate", host)
		}
	}
}

func TestValidateWarnsSelfHostedButAdmits(t *testing.T) {
	input := validInput()
	input["api_origin"] = str("https://sentry.internal.acme.example")
	response := validate(t, input)
	if !response.GetValid() {
		t.Fatal("a self-hosted origin is admissible input, not an error")
	}
	if !hasDiagnostic(response.GetDiagnostics(), DiagSelfHostedAdmission) {
		t.Fatal("a self-hosted origin must warn that explicit governed admission is required")
	}
}

func TestValidateRejectsSecretShapedInput(t *testing.T) {
	input := validInput()
	input["environment"] = str("bearer sk_live_0123456789abcdef")
	if validate(t, input).GetValid() {
		t.Fatal("a secret-shaped literal must never be accepted as input")
	}
}

func TestValidateRejectsSecretBearingDSN(t *testing.T) {
	input := validInput()
	input["expected_dsn"] = str("https://public:secret@sentry.io/1")
	if validate(t, input).GetValid() {
		t.Fatal("a DSN carrying a password (secret) must be rejected; only the public DSN may be supplied")
	}
}

func TestValidateAcceptsPublicDSN(t *testing.T) {
	input := validInput()
	input["expected_dsn"] = str("https://abc123@sentry.io/42")
	if !validate(t, input).GetValid() {
		t.Fatal("a public DSN must be accepted")
	}
}
