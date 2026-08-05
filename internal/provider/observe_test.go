package provider

import (
	"bytes"
	"strings"
	"testing"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"google.golang.org/protobuf/proto"
)

const (
	keyID1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	keyID2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	keyID3 = "cccccccccccccccccccccccccccccccc"

	legacySecret = "srt_legacy_client_key_secret"
	dsnSecret    = "https://pub1:srt_dsn_secret@sentry.io/1"

	projectBody = `{"id":"1","slug":"web","name":"Web","status":"active","platform":"node","organization":{"slug":"acme","id":"9"}}`
)

// clientKey renders one Sentry client-key object carrying both legacy secret
// locations beside the public DSN, exactly as Sentry's list endpoint returns.
func clientKey(id, publicDSN string, active bool) string {
	state := "true"
	if !active {
		state = "false"
	}
	return `{"id":"` + id + `","name":"Key","isActive":` + state + `,"public":"pub",` +
		`"secret":"` + legacySecret + `","rateLimit":{"window":60,"count":100},` +
		`"dsn":{"public":"` + publicDSN + `","secret":"` + dsnSecret + `"}}`
}

func clientKeyList(objects ...string) string {
	return "[" + strings.Join(objects, ",") + "]"
}

func observe(t *testing.T, host Host, input map[string]*providerv0.PublicValue) *providerv0.ObserveResponse {
	t.Helper()
	response, err := hostServer(t, host).Observe(t.Context(), &providerv0.ObserveRequest{Context: hostContext(input)})
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	return response
}

// assertNoSecrets fails if any legacy secret byte or the opaque setup handle
// survives into the observation the provider returns.
func assertNoSecrets(t *testing.T, response *providerv0.ObserveResponse) {
	t.Helper()
	raw, err := proto.Marshal(response)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{legacySecret, "srt_dsn_secret", "setup-handle"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("forbidden value %q leaked into observation", forbidden)
		}
	}
}

func TestObserveProjectAccessibility(t *testing.T) {
	host := newFakeHost(loadManifest(t)).
		set(requestProjectRetrieve, 200, projectBody).
		set(requestClientKeyList, 200, clientKeyList(clientKey(keyID1, "https://pub1@sentry.io/1", true)))

	response := observe(t, host, validInput())
	material := response.GetMaterial()
	if !material.GetComplete() {
		t.Fatal("a fully answered observation must be complete")
	}
	project := observedProjectFrom(material)
	if project == nil || !project.Accessible || project.Slug != "web" || project.OrgSlug != "acme" {
		t.Fatalf("project not observed as accessible/identity-matched: %+v", project)
	}
	assertNoSecrets(t, response)
}

func TestObserveSuppressesClientKeySecrets(t *testing.T) {
	host := newFakeHost(loadManifest(t)).
		set(requestProjectRetrieve, 200, projectBody).
		set(requestClientKeyList, 200, clientKeyList(clientKey(keyID1, "https://pub1@sentry.io/1", true)))

	response := observe(t, host, validInput())
	keys := observedClientKeys(response.GetMaterial())
	if len(keys) != 1 || keys[0].PublicDSN != "https://pub1@sentry.io/1" {
		t.Fatalf("public DSN not forwarded: %+v", keys)
	}
	assertNoSecrets(t, response)
}

func TestObservePaginatedClientKeysAggregateInOrder(t *testing.T) {
	host := newFakeHost(loadManifest(t)).
		set(requestProjectRetrieve, 200, projectBody).
		set(requestClientKeyList, 200, clientKeyList(
			clientKey(keyID1, "https://pub1@sentry.io/1", false),
			clientKey(keyID2, "https://pub2@sentry.io/1", true),
			clientKey(keyID3, "https://pub3@sentry.io/1", false),
		))

	response := observe(t, host, validInput())
	keys := observedClientKeys(response.GetMaterial())
	if len(keys) != 3 {
		t.Fatalf("expected 3 keys observed, got %d", len(keys))
	}
	if keys[0].ID != keyID1 || keys[1].ID != keyID2 || keys[2].ID != keyID3 {
		t.Fatalf("keys not in cursor order: %+v", keys)
	}
	// Selection must pick the sole active key, never array[0].
	sel := selectClientKey(keys, "")
	if sel.Outcome != selectionSelected || sel.Key.ID != keyID2 {
		t.Fatalf("selection did not pick the sole active key: %+v", sel)
	}
	assertNoSecrets(t, response)
}

func TestObserveAuthErrorYieldsNoProject(t *testing.T) {
	host := newFakeHost(loadManifest(t)).
		set(requestProjectRetrieve, 401, "").
		set(requestClientKeyList, 401, "")

	response := observe(t, host, validInput())
	if observedProjectFrom(response.GetMaterial()) != nil {
		t.Fatal("an unauthenticated read must not yield a project observation")
	}
	if !hasDiagnostic(response.GetVolatile().GetDiagnostics(), DiagAuthentication) {
		t.Fatal("an authentication diagnostic must be surfaced")
	}
}

func TestObservePermissionErrorIsScoped(t *testing.T) {
	host := newFakeHost(loadManifest(t)).
		set(requestProjectRetrieve, 403, "").
		set(requestClientKeyList, 403, "")

	response := observe(t, host, validInput())
	if !hasDiagnostic(response.GetVolatile().GetDiagnostics(), DiagPermission) {
		t.Fatal("a permission diagnostic must be surfaced")
	}
}

func TestObserveRateLimitIsIncomplete(t *testing.T) {
	host := newFakeHost(loadManifest(t)).
		set(requestProjectRetrieve, 200, projectBody).
		set(requestClientKeyList, 429, "")

	response := observe(t, host, validInput())
	if response.GetMaterial().GetComplete() {
		t.Fatal("a rate-limited read must yield an incomplete observation")
	}
	if response.GetVolatile().GetRetryAfter() == nil {
		t.Fatal("a rate limit must surface a retry-after hint")
	}
	if !hasDiagnostic(response.GetVolatile().GetDiagnostics(), DiagRateLimit) {
		t.Fatal("a rate-limit diagnostic must be surfaced")
	}
}

func TestObserveExtraSecretFieldDoesNotLeak(t *testing.T) {
	// An unexpected, undeclared secret field beside the declared ones must be
	// dropped, never forwarded.
	poison := `[{"id":"` + keyID1 + `","isActive":true,"public":"pub",` +
		`"secret":"` + legacySecret + `","surprise_token":"srt_undeclared_secret",` +
		`"dsn":{"public":"https://pub1@sentry.io/1","secret":"` + dsnSecret + `"}}]`
	host := newFakeHost(loadManifest(t)).
		set(requestProjectRetrieve, 200, projectBody).
		set(requestClientKeyList, 200, poison)

	response := observe(t, host, validInput())
	raw, _ := proto.Marshal(response)
	if bytes.Contains(raw, []byte("srt_undeclared_secret")) {
		t.Fatal("an undeclared secret field leaked into the observation")
	}
	assertNoSecrets(t, response)
}

func TestObserveFailsClosedOnMovedSecret(t *testing.T) {
	// Under a strict policy the legacy dsn.secret is renamed away, so the
	// required suppression can no longer confirm it was handled: the host fails
	// closed and the provider surfaces an incomplete, safe observation.
	moved := `[{"id":"` + keyID1 + `","isActive":true,"public":"pub",` +
		`"secret":"` + legacySecret + `",` +
		`"dsn":{"public":"https://pub1@sentry.io/1","sekret":"` + dsnSecret + `"}}]`
	host := newFakeHost(loadManifest(t)).
		set(requestProjectRetrieve, 200, projectBody).
		set(requestClientKeyList, 200, moved)
	host.strictSuppress = true

	response := observe(t, host, validInput())
	if response.GetMaterial().GetComplete() {
		t.Fatal("a fail-closed read must yield an incomplete observation")
	}
	if len(observedClientKeys(response.GetMaterial())) != 0 {
		t.Fatal("no client keys must be observed when the response fails closed")
	}
	assertNoSecrets(t, response)
}

func TestObserveMalformedResponseFailsSafe(t *testing.T) {
	// A body the host cannot parse as declared JSON fails filtering closed; the
	// provider must surface an incomplete, empty-keyed observation, never a guess.
	host := newFakeHost(loadManifest(t)).
		set(requestProjectRetrieve, 200, projectBody).
		set(requestClientKeyList, 200, `[{"id":"aaa","id":"bbb"}]`) // duplicate keys are rejected

	response := observe(t, host, validInput())
	if response.GetMaterial().GetComplete() {
		t.Fatal("a malformed response must yield an incomplete observation")
	}
	if len(observedClientKeys(response.GetMaterial())) != 0 {
		t.Fatal("no keys must be observed from a malformed response")
	}
}

func TestObserveContinuesFromCursor(t *testing.T) {
	// When the host resumes an observation with a cursor, the provider forwards
	// it as the descriptor's cursor query field and reads that page.
	host := newFakeHost(loadManifest(t)).
		set(requestProjectRetrieve, 200, projectBody).
		setCursor(requestClientKeyList, "page2", 200, clientKeyList(clientKey(keyID2, "https://pub2@sentry.io/1", true)))

	response, err := hostServer(t, host).Observe(t.Context(), &providerv0.ObserveRequest{
		Context: hostContext(validInput()),
		Cursor:  "page2",
	})
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	keys := observedClientKeys(response.GetMaterial())
	if len(keys) != 1 || keys[0].ID != keyID2 {
		t.Fatalf("cursor page not read: %+v", keys)
	}
}

func TestObserveRequiresHost(t *testing.T) {
	server := testServer(t) // no host wired
	_, err := server.Observe(t.Context(), &providerv0.ObserveRequest{Context: hostContext(validInput())})
	if err == nil {
		t.Fatal("observe without a host callback must fail closed")
	}
}
