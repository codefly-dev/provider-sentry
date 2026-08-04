package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
)

const (
	orgProjectsPath = "/api/0/organizations/acme/projects/"
	orgKeysPath     = "/api/0/organizations/acme/project-keys/"
)

// keyC extends the plan_test key ids for the multiple-active-key fixtures.
var keyC = strings.Repeat("c", 32)

// observeInput is a well-formed org/project input on the default SaaS origin.
func observeInput() map[string]*providerv0.PublicValue {
	return map[string]*providerv0.PublicValue{
		"api_origin":        str("https://sentry.io"),
		"organization_slug": str("acme"),
		"project_slug":      str("web"),
		"environment":       str("production"),
	}
}

func jsonRoute(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, body) }
}

// observeMaterial records then replays one Observe and returns the replayed
// material observation. Replay serves filtered responses with no network
// reachable and fails if the cassette persisted any secret.
func observeMaterial(t *testing.T, routes map[string]http.HandlerFunc, input map[string]*providerv0.PublicValue, cursor string) (*fakeHost, *providerv0.MaterialObservation) {
	t.Helper()
	host := newFakeHost(t)
	host.record(sentryStub(t, routes))
	server := fakeServer(t, host)

	ctx := brokerContext(t, "op-observe", "act-observe", input)
	if _, err := server.Observe(context.Background(), &providerv0.ObserveRequest{Context: ctx, Cursor: cursor}); err != nil {
		t.Fatalf("observe (record): %v", err)
	}
	host.replay()
	response, err := server.Observe(context.Background(), &providerv0.ObserveRequest{Context: ctx, Cursor: cursor})
	if err != nil {
		t.Fatalf("observe (replay): %v", err)
	}
	return host, response.GetMaterial()
}

func projectFixture() string {
	return projectListJSON(
		projectJSON("11", "web", "Web", "active"),
		projectJSON("22", "api", "API", "active"),
	)
}

func TestObserveProjectAndOwnedActiveKey(t *testing.T) {
	routes := map[string]http.HandlerFunc{
		orgProjectsPath: jsonRoute(projectFixture()),
		orgKeysPath: jsonRoute(clientKeyListJSON(
			clientKeyJSON(keyA, "11", true, "https://pub_a@sentry.io/11"),
			clientKeyJSON(keyB, "22", true, "https://pub_b@sentry.io/22"),
		)),
	}
	_, material := observeMaterial(t, routes, observeInput(), "")

	project := observedProjectFrom(material)
	if project == nil || !project.Accessible {
		t.Fatalf("expected an accessible project, got %+v", project)
	}
	if project.Slug != "web" || project.OrgSlug != "acme" || project.RemoteID != "11" {
		t.Fatalf("unexpected project identity: %+v", project)
	}

	// Only the selected project's key is kept; the sibling project's key is
	// filtered out by projectId.
	keys := observedClientKeys(material)
	if len(keys) != 1 || keys[0].ID != keyA {
		t.Fatalf("expected exactly the selected project's key, got %+v", keys)
	}
	if keys[0].PublicDSN != "https://pub_a@sentry.io/11" || !keys[0].Active {
		t.Fatalf("unexpected client-key projection: %+v", keys[0])
	}
	if !material.GetComplete() {
		t.Fatal("observation should be complete")
	}
}

func TestObserveProjectsToRuntimeProjection(t *testing.T) {
	routes := map[string]http.HandlerFunc{
		orgProjectsPath: jsonRoute(projectFixture()),
		orgKeysPath:     jsonRoute(clientKeyListJSON(clientKeyJSON(keyA, "11", true, "https://pub_a@sentry.io/11"))),
	}
	host, material := observeMaterial(t, routes, observeInput(), "")

	// The observed material drives the offline Plan: a sole active key projects
	// error-tracking@1 with the public DSN.
	response := plan(t, planRequest(observeInput(), material, runtimeTarget()))
	if !hasActionType(response, providerv0.ActionType_ACTION_TYPE_PROJECT_OUTPUT) {
		t.Fatalf("expected a PROJECT_OUTPUT action, got %v", actionTypes(response))
	}
	_ = host
}

func TestObserveSuppressesClientKeySecrets(t *testing.T) {
	routes := map[string]http.HandlerFunc{
		orgProjectsPath: jsonRoute(projectFixture()),
		orgKeysPath:     jsonRoute(clientKeyListJSON(clientKeyJSON(keyA, "11", true, "https://pub_a@sentry.io/11"))),
	}
	host, material := observeMaterial(t, routes, observeInput(), "")

	// The public DSN survives; neither legacy secret location appears anywhere in
	// the projected observation.
	keys := observedClientKeys(material)
	if len(keys) != 1 || keys[0].PublicDSN == "" {
		t.Fatalf("expected the public DSN to be forwarded, got %+v", keys)
	}
	assertNoSecret(t, material)
	// The capture sink was never invoked (asserted by captureSink), and the
	// cassette persisted no secret (asserted by host.replay()).
	_ = host
}

func TestObserveZeroActiveKeysBlocksSelection(t *testing.T) {
	routes := map[string]http.HandlerFunc{
		orgProjectsPath: jsonRoute(projectFixture()),
		orgKeysPath:     jsonRoute(clientKeyListJSON(clientKeyJSON(keyA, "11", false, "https://pub_a@sentry.io/11"))),
	}
	_, material := observeMaterial(t, routes, observeInput(), "")

	response := plan(t, planRequest(observeInput(), material, runtimeTarget()))
	if !hasDiagnostic(response.GetDiagnostics(), DiagNoActiveKey) {
		t.Fatalf("expected a no-active-key manual action, got %v", response.GetDiagnostics())
	}
}

func TestObserveMultipleActiveKeysBlocksSelection(t *testing.T) {
	routes := map[string]http.HandlerFunc{
		orgProjectsPath: jsonRoute(projectFixture()),
		orgKeysPath: jsonRoute(clientKeyListJSON(
			clientKeyJSON(keyA, "11", true, "https://pub_a@sentry.io/11"),
			clientKeyJSON(keyC, "11", true, "https://pub_c@sentry.io/11"),
		)),
	}
	_, material := observeMaterial(t, routes, observeInput(), "")

	response := plan(t, planRequest(observeInput(), material, runtimeTarget()))
	if !hasDiagnostic(response.GetDiagnostics(), DiagAmbiguousKey) {
		t.Fatalf("expected an ambiguous-key block, got %v", response.GetDiagnostics())
	}
}

func TestObserveRevokedExplicitKeyBlocks(t *testing.T) {
	routes := map[string]http.HandlerFunc{
		orgProjectsPath: jsonRoute(projectFixture()),
		orgKeysPath:     jsonRoute(clientKeyListJSON(clientKeyJSON(keyA, "11", false, "https://pub_a@sentry.io/11"))),
	}
	input := observeInput()
	input["client_key_id"] = str(keyA)
	_, material := observeMaterial(t, routes, input, "")

	response := plan(t, planRequest(input, material, runtimeTarget()))
	if !hasDiagnostic(response.GetDiagnostics(), DiagKeyRevoked) {
		t.Fatalf("expected a revoked-key block, got %v", response.GetDiagnostics())
	}
}

func TestObserveInaccessibleProjectBlocks(t *testing.T) {
	routes := map[string]http.HandlerFunc{
		// The configured "web" project is absent from what the credential can read.
		orgProjectsPath: jsonRoute(projectListJSON(projectJSON("22", "api", "API", "active"))),
		orgKeysPath:     jsonRoute(clientKeyListJSON()),
	}
	_, material := observeMaterial(t, routes, observeInput(), "")

	project := observedProjectFrom(material)
	if project == nil || project.Accessible {
		t.Fatalf("expected an inaccessible project projection, got %+v", project)
	}
	response := plan(t, planRequest(observeInput(), material, runtimeTarget()))
	if !hasDiagnostic(response.GetDiagnostics(), DiagProjectInaccessible) {
		t.Fatalf("expected a project-inaccessible block, got %v", response.GetDiagnostics())
	}
}

func TestObservePassesCursorThrough(t *testing.T) {
	var keysCursor string
	routes := map[string]http.HandlerFunc{
		orgProjectsPath: jsonRoute(projectFixture()),
		orgKeysPath: func(w http.ResponseWriter, r *http.Request) {
			keysCursor = r.URL.Query().Get("cursor")
			writeJSON(w, http.StatusOK, clientKeyListJSON(clientKeyJSON(keyA, "11", true, "https://pub_a@sentry.io/11")))
		},
	}
	host := newFakeHost(t)
	host.record(sentryStub(t, routes))
	server := fakeServer(t, host)
	ctx := brokerContext(t, "op-cursor", "act-cursor", observeInput())
	if _, err := server.Observe(context.Background(), &providerv0.ObserveRequest{Context: ctx, Cursor: "100:1:0"}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	if keysCursor != "100:1:0" {
		t.Fatalf("expected the host cursor to reach the client-key request, got %q", keysCursor)
	}
}

func TestObserveAuthErrorFailsClosed(t *testing.T) {
	host := newFakeHost(t)
	host.record(sentryStub(t, map[string]http.HandlerFunc{
		orgProjectsPath: func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusUnauthorized, `{}`) },
		orgKeysPath:     jsonRoute(clientKeyListJSON()),
	}))
	server := fakeServer(t, host)
	ctx := brokerContext(t, "op-auth", "act-auth", observeInput())
	_, err := server.Observe(context.Background(), &providerv0.ObserveRequest{Context: ctx})
	if err == nil {
		t.Fatal("an authentication failure must fail the observation closed")
	}
}

func TestObserveRateLimitFailsClosed(t *testing.T) {
	host := newFakeHost(t)
	host.record(sentryStub(t, map[string]http.HandlerFunc{
		orgProjectsPath: jsonRoute(projectFixture()),
		orgKeysPath:     func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusTooManyRequests, `{}`) },
	}))
	server := fakeServer(t, host)
	ctx := brokerContext(t, "op-429", "act-429", observeInput())
	_, err := server.Observe(context.Background(), &providerv0.ObserveRequest{Context: ctx})
	if err == nil {
		t.Fatal("a 429 must fail the observation closed")
	}
}

// TestPlanBlocksIncompleteObservation is the truncated/incomplete-cursor safety
// failure: a client-key list that could not be fully enumerated must block
// selection rather than risk a silent wrong choice on a truncated page.
func TestPlanBlocksIncompleteObservation(t *testing.T) {
	incomplete := &providerv0.MaterialObservation{
		Complete:   false,
		NextCursor: "100:1:0",
		Resources: []*providerv0.MaterialResourceObservation{
			projectResource(true, "acme", "web"),
			clientKeyResource(keyA, true, "https://pub_a@sentry.io/11"),
		},
	}
	response := plan(t, planRequest(observeInput(), incomplete, runtimeTarget()))
	if !hasDiagnostic(response.GetDiagnostics(), DiagObservationIncomplete) {
		t.Fatalf("expected an incomplete-observation block, got %v", response.GetDiagnostics())
	}
	if !hasActionType(response, providerv0.ActionType_ACTION_TYPE_BLOCKED) {
		t.Fatalf("expected a BLOCKED action, got %v", actionTypes(response))
	}
}

// assertNoSecret walks every projected value and fails if a suppressed secret
// leaked into the observation.
func assertNoSecret(t *testing.T, material *providerv0.MaterialObservation) {
	t.Helper()
	for _, resource := range material.GetResources() {
		for key, value := range resource.GetProviderOwnedFields() {
			for _, needle := range []string{legacySecret, dsnSecret, "srt_dsn_secret_value"} {
				if s, ok := publicString(value); ok && strings.Contains(s, needle) {
					t.Fatalf("suppressed secret leaked into field %q: %q", key, s)
				}
			}
		}
	}
}
