package provider

import (
	"context"
	"net/http"
	"testing"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
)

func runDoctor(t *testing.T, routes map[string]http.HandlerFunc, input map[string]*providerv0.PublicValue) *providerv0.DoctorResponse {
	t.Helper()
	host := newFakeHost(t)
	host.record(sentryStub(t, routes))
	server := fakeServer(t, host)
	ctx := brokerContext(t, "op-doctor", "act-doctor", input)
	response, err := server.Doctor(context.Background(), &providerv0.DoctorRequest{Context: ctx})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	return response
}

func TestDoctorHealthyWithSoleActiveKey(t *testing.T) {
	response := runDoctor(t, map[string]http.HandlerFunc{
		orgProjectsPath: jsonRoute(projectFixture()),
		orgKeysPath:     jsonRoute(clientKeyListJSON(clientKeyJSON(keyA, "11", true, "https://pub_a@sentry.io/11"))),
	}, observeInput())
	if !response.GetHealthy() {
		t.Fatalf("expected healthy, got diagnostics %v", response.GetDiagnostics())
	}
}

func TestDoctorCredentialUnreachable(t *testing.T) {
	response := runDoctor(t, map[string]http.HandlerFunc{
		orgProjectsPath: func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusUnauthorized, `{}`) },
		orgKeysPath:     jsonRoute(clientKeyListJSON()),
	}, observeInput())
	if response.GetHealthy() {
		t.Fatal("an unauthenticated credential must report unhealthy")
	}
	if !hasDiagnostic(response.GetDiagnostics(), DiagAuthentication) {
		t.Fatalf("expected an authentication diagnostic, got %v", response.GetDiagnostics())
	}
}

func TestDoctorProjectNotReadable(t *testing.T) {
	response := runDoctor(t, map[string]http.HandlerFunc{
		orgProjectsPath: jsonRoute(projectListJSON(projectJSON("22", "api", "API", "active"))),
		orgKeysPath:     jsonRoute(clientKeyListJSON()),
	}, observeInput())
	if response.GetHealthy() {
		t.Fatal("a project the credential cannot read must report unhealthy")
	}
	if !hasDiagnostic(response.GetDiagnostics(), DiagProjectInaccessible) {
		t.Fatalf("expected a project-inaccessible diagnostic, got %v", response.GetDiagnostics())
	}
}

func TestDoctorNoActiveKeyWarns(t *testing.T) {
	response := runDoctor(t, map[string]http.HandlerFunc{
		orgProjectsPath: jsonRoute(projectFixture()),
		orgKeysPath:     jsonRoute(clientKeyListJSON(clientKeyJSON(keyA, "11", false, "https://pub_a@sentry.io/11"))),
	}, observeInput())
	// A missing active key is a warning, not an error: the account is reachable.
	if !response.GetHealthy() {
		t.Fatalf("a warning must not make the provider unhealthy: %v", response.GetDiagnostics())
	}
	if !hasDiagnostic(response.GetDiagnostics(), DiagNoActiveKey) {
		t.Fatalf("expected a no-active-key warning, got %v", response.GetDiagnostics())
	}
}

func TestDoctorAmbiguousKeyWarns(t *testing.T) {
	response := runDoctor(t, map[string]http.HandlerFunc{
		orgProjectsPath: jsonRoute(projectFixture()),
		orgKeysPath: jsonRoute(clientKeyListJSON(
			clientKeyJSON(keyA, "11", true, "https://pub_a@sentry.io/11"),
			clientKeyJSON(keyC, "11", true, "https://pub_c@sentry.io/11"),
		)),
	}, observeInput())
	if !hasDiagnostic(response.GetDiagnostics(), DiagAmbiguousKey) {
		t.Fatalf("expected an ambiguous-key warning, got %v", response.GetDiagnostics())
	}
}

func TestDoctorWithoutHostFailsClosed(t *testing.T) {
	server := testServer(t) // no host attached
	_, err := server.Doctor(context.Background(), &providerv0.DoctorRequest{
		Context: brokerContext(t, "op", "act", observeInput()),
	})
	if err == nil {
		t.Fatal("doctor must fail closed without a host callback channel")
	}
}
