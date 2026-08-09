package provider

import (
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
)

func diagnosticSeverity(diagnostics []*basev0.FailureDiagnostic, code string) basev0.FailureDiagnostic_Severity {
	for _, d := range diagnostics {
		if d.GetCode() == code {
			return d.GetSeverity()
		}
	}
	return basev0.FailureDiagnostic_SEVERITY_UNSPECIFIED
}

func doctor(t *testing.T, host Host, input map[string]*providerv0.PublicValue) *providerv0.DoctorResponse {
	t.Helper()
	response, err := hostServer(t, host).Doctor(t.Context(), &providerv0.DoctorRequest{Context: hostContext(input)})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	return response
}

func TestDoctorHealthyWhenReadyAndUnambiguous(t *testing.T) {
	host := newFakeHost(loadManifest(t)).
		set(requestProjectRetrieve, 200, projectBody).
		set(requestClientKeyList, 200, clientKeyList(clientKey(keyID1, "https://pub1@sentry.io/1", true)))

	response := doctor(t, host, validInput())
	if !response.GetHealthy() {
		t.Fatalf("expected healthy, got diagnostics %v", response.GetDiagnostics())
	}
}

func TestDoctorReportsCredentialUnreachable(t *testing.T) {
	host := newFakeHost(loadManifest(t)).set(requestProjectRetrieve, 401, "")
	response := doctor(t, host, validInput())
	if response.GetHealthy() {
		t.Fatal("an unauthenticated credential is not healthy")
	}
	if !hasDiagnostic(response.GetDiagnostics(), DiagAuthentication) {
		t.Fatal("expected an authentication diagnostic")
	}
}

func TestDoctorReportsMissingScope(t *testing.T) {
	host := newFakeHost(loadManifest(t)).set(requestProjectRetrieve, 403, "")
	response := doctor(t, host, validInput())
	if response.GetHealthy() || !hasDiagnostic(response.GetDiagnostics(), DiagPermission) {
		t.Fatal("a scope-denied read must be reported as a permission failure")
	}
}

func TestDoctorReportsProjectNotFound(t *testing.T) {
	host := newFakeHost(loadManifest(t)).set(requestProjectRetrieve, 404, "")
	response := doctor(t, host, validInput())
	if response.GetHealthy() || !hasDiagnostic(response.GetDiagnostics(), DiagNotFound) {
		t.Fatal("a missing project must be reported")
	}
}

func TestDoctorReportsIdentityMismatch(t *testing.T) {
	mismatched := `{"id":"1","slug":"other","name":"Other","status":"active","organization":{"slug":"acme","id":"9"}}`
	host := newFakeHost(loadManifest(t)).
		set(requestProjectRetrieve, 200, mismatched).
		set(requestClientKeyList, 200, clientKeyList(clientKey(keyID1, "https://pub1@sentry.io/1", true)))
	response := doctor(t, host, validInput())
	if response.GetHealthy() || !hasDiagnostic(response.GetDiagnostics(), DiagProjectInaccessible) {
		t.Fatal("a project whose identity does not match must be reported")
	}
}

func TestDoctorReportsNoActiveKey(t *testing.T) {
	host := newFakeHost(loadManifest(t)).
		set(requestProjectRetrieve, 200, projectBody).
		set(requestClientKeyList, 200, clientKeyList(clientKey(keyID1, "https://pub1@sentry.io/1", false)))
	response := doctor(t, host, validInput())
	if response.GetHealthy() || !hasDiagnostic(response.GetDiagnostics(), DiagNoActiveKey) {
		t.Fatal("a project with no active key must be reported")
	}
}

func TestDoctorReportsAmbiguousKeys(t *testing.T) {
	host := newFakeHost(loadManifest(t)).
		set(requestProjectRetrieve, 200, projectBody).
		set(requestClientKeyList, 200, clientKeyList(
			clientKey(keyID1, "https://pub1@sentry.io/1", true),
			clientKey(keyID2, "https://pub2@sentry.io/1", true),
		))
	response := doctor(t, host, validInput())
	if response.GetHealthy() || !hasDiagnostic(response.GetDiagnostics(), DiagAmbiguousKey) {
		t.Fatal("multiple active keys must be reported as ambiguous")
	}
}

func TestDoctorReportsRevokedExplicitKey(t *testing.T) {
	input := validInput()
	input["client_key_id"] = str(keyID1)
	host := newFakeHost(loadManifest(t)).
		set(requestProjectRetrieve, 200, projectBody).
		set(requestClientKeyList, 200, clientKeyList(clientKey(keyID1, "https://pub1@sentry.io/1", false)))
	response := doctor(t, host, input)
	if response.GetHealthy() || !hasDiagnostic(response.GetDiagnostics(), DiagKeyRevoked) {
		t.Fatal("an explicitly configured revoked key must be reported")
	}
}

func TestDoctorRateLimitIsTransient(t *testing.T) {
	host := newFakeHost(loadManifest(t)).set(requestProjectRetrieve, 429, "")
	response := doctor(t, host, validInput())
	if response.GetHealthy() {
		t.Fatal("a rate-limited doctor cannot confirm health")
	}
	if diagnosticSeverity(response.GetDiagnostics(), DiagRateLimit) != basev0.FailureDiagnostic_WARNING {
		t.Fatal("a rate limit is a transient warning, not a hard error")
	}
}

func TestDoctorRequiresHost(t *testing.T) {
	server := testServer(t)
	if _, err := server.Doctor(t.Context(), &providerv0.DoctorRequest{Context: hostContext(validInput())}); err == nil {
		t.Fatal("doctor without a host callback must fail closed")
	}
}
