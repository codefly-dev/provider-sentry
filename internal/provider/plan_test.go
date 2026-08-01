package provider

import (
	"bytes"
	"strings"
	"testing"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"google.golang.org/protobuf/proto"
)

var (
	keyA = strings.Repeat("a", 32)
	keyB = strings.Repeat("b", 32)
	dsnA = "https://aaa@sentry.io/1"
	dsnB = "https://bbb@sentry.io/1"
)

func findAction(response *providerv0.PlanResponse, want providerv0.ActionType) *providerv0.PlanAction {
	for _, a := range response.GetPlan().GetActions() {
		if a.GetType() == want {
			return a
		}
	}
	return nil
}

func TestPlanSelectsSoleActiveKeyAndProjectsDSN(t *testing.T) {
	obs := observation(projectResource(true, "acme", "web"), clientKeyResource(keyA, true, dsnA))
	response := plan(t, planRequest(validInput(), obs, runtimeTarget()))

	if !hasActionType(response, providerv0.ActionType_ACTION_TYPE_NO_OP) {
		t.Fatal("a uniquely selected active key is a NO_OP (observe-only)")
	}
	project := findAction(response, providerv0.ActionType_ACTION_TYPE_PROJECT_OUTPUT)
	if project == nil {
		t.Fatal("expected a PROJECT_OUTPUT action")
	}
	dsn := project.GetOutput().GetValues()["SENTRY_DSN"].GetPublicValue().GetStringValue()
	if dsn != dsnA {
		t.Fatalf("projected DSN = %q, want %q", dsn, dsnA)
	}
	if project.GetOutput().GetValues()["SENTRY_ENVIRONMENT"].GetPublicValue().GetStringValue() != "production" {
		t.Fatal("environment must be projected when supplied")
	}
}

// TestPlanNeverSelectsArrayZeroOnAmbiguity proves selection blocks on multiple
// active keys instead of grabbing the first array element.
func TestPlanNeverSelectsArrayZeroOnAmbiguity(t *testing.T) {
	obs := observation(projectResource(true, "acme", "web"),
		clientKeyResource(keyA, true, dsnA), clientKeyResource(keyB, true, dsnB))
	response := plan(t, planRequest(validInput(), obs, runtimeTarget()))
	if !hasActionType(response, providerv0.ActionType_ACTION_TYPE_BLOCKED) {
		t.Fatal("multiple active keys must BLOCK, not select array[0]")
	}
	if hasActionType(response, providerv0.ActionType_ACTION_TYPE_PROJECT_OUTPUT) {
		t.Fatal("an ambiguous selection must not project a DSN")
	}
	if !hasDiagnostic(response.GetDiagnostics(), DiagAmbiguousKey) {
		t.Fatal("ambiguity must report both safe key ids")
	}
}

// TestPlanSkipsInactiveLeadingKey proves the sole active key is chosen even when
// an inactive key precedes it in the array.
func TestPlanSkipsInactiveLeadingKey(t *testing.T) {
	obs := observation(projectResource(true, "acme", "web"),
		clientKeyResource(keyA, false, dsnA), clientKeyResource(keyB, true, dsnB))
	response := plan(t, planRequest(validInput(), obs, runtimeTarget()))
	project := findAction(response, providerv0.ActionType_ACTION_TYPE_PROJECT_OUTPUT)
	if project == nil {
		t.Fatal("the sole active key must be selected")
	}
	if got := project.GetOutput().GetValues()["SENTRY_DSN"].GetPublicValue().GetStringValue(); got != dsnB {
		t.Fatalf("selected DSN = %q, want the active key's DSN %q", got, dsnB)
	}
}

func TestPlanManualWhenNoActiveKey(t *testing.T) {
	obs := observation(projectResource(true, "acme", "web"), clientKeyResource(keyA, false, dsnA))
	response := plan(t, planRequest(validInput(), obs, runtimeTarget()))
	if !hasActionType(response, providerv0.ActionType_ACTION_TYPE_MANUAL) {
		t.Fatal("zero active keys must be a MANUAL_ACTION")
	}
	if !hasDiagnostic(response.GetDiagnostics(), DiagNoActiveKey) {
		t.Fatal("zero active keys must report a no-active-key diagnostic")
	}
}

func TestPlanExplicitKeySelection(t *testing.T) {
	input := validInput()
	input["client_key_id"] = str(keyB)
	obs := observation(projectResource(true, "acme", "web"),
		clientKeyResource(keyA, true, dsnA), clientKeyResource(keyB, true, dsnB))
	response := plan(t, planRequest(input, obs, runtimeTarget()))
	project := findAction(response, providerv0.ActionType_ACTION_TYPE_PROJECT_OUTPUT)
	if project == nil {
		t.Fatal("an explicit active key wins even when others are active")
	}
	if got := project.GetOutput().GetValues()["SENTRY_DSN"].GetPublicValue().GetStringValue(); got != dsnB {
		t.Fatalf("explicit selection projected %q, want %q", got, dsnB)
	}
}

func TestPlanBlocksExplicitMissing(t *testing.T) {
	input := validInput()
	input["client_key_id"] = str(keyB)
	obs := observation(projectResource(true, "acme", "web"), clientKeyResource(keyA, true, dsnA))
	response := plan(t, planRequest(input, obs, runtimeTarget()))
	if !hasActionType(response, providerv0.ActionType_ACTION_TYPE_BLOCKED) || !hasDiagnostic(response.GetDiagnostics(), DiagKeyMismatch) {
		t.Fatal("an explicit key not owned by the project must BLOCK")
	}
}

func TestPlanBlocksExplicitRevoked(t *testing.T) {
	input := validInput()
	input["client_key_id"] = str(keyA)
	obs := observation(projectResource(true, "acme", "web"), clientKeyResource(keyA, false, dsnA))
	response := plan(t, planRequest(input, obs, runtimeTarget()))
	if !hasActionType(response, providerv0.ActionType_ACTION_TYPE_BLOCKED) || !hasDiagnostic(response.GetDiagnostics(), DiagKeyRevoked) {
		t.Fatal("an explicit revoked key must BLOCK with a revocation diagnostic")
	}
}

func TestPlanBlocksInaccessibleProject(t *testing.T) {
	obs := observation(projectResource(false, "acme", "web"), clientKeyResource(keyA, true, dsnA))
	response := plan(t, planRequest(validInput(), obs, runtimeTarget()))
	if !hasActionType(response, providerv0.ActionType_ACTION_TYPE_BLOCKED) || !hasDiagnostic(response.GetDiagnostics(), DiagProjectInaccessible) {
		t.Fatal("an inaccessible project must BLOCK before any projection")
	}
	if hasActionType(response, providerv0.ActionType_ACTION_TYPE_PROJECT_OUTPUT) {
		t.Fatal("an inaccessible project must not project")
	}
}

func TestPlanBlocksProjectIdentityMismatch(t *testing.T) {
	obs := observation(projectResource(true, "acme", "other-project"), clientKeyResource(keyA, true, dsnA))
	response := plan(t, planRequest(validInput(), obs, runtimeTarget()))
	if !hasActionType(response, providerv0.ActionType_ACTION_TYPE_BLOCKED) {
		t.Fatal("an observed project that is not the configured one must BLOCK, never be adopted")
	}
}

func TestPlanDSNMismatchWarnsButProjects(t *testing.T) {
	input := validInput()
	input["expected_dsn"] = str("https://different@sentry.io/9")
	obs := observation(projectResource(true, "acme", "web"), clientKeyResource(keyA, true, dsnA))
	response := plan(t, planRequest(input, obs, runtimeTarget()))
	if !hasDiagnostic(response.GetDiagnostics(), DiagDSNMismatch) {
		t.Fatal("a supplied DSN that differs from the selected key must warn")
	}
	if !hasActionType(response, providerv0.ActionType_ACTION_TYPE_PROJECT_OUTPUT) {
		t.Fatal("the authoritative observed DSN is still projected")
	}
}

func TestPlanProjectsBuildContract(t *testing.T) {
	obs := observation(projectResource(true, "acme", "web"))
	buildRef := &providerv0.OpaqueReference{Reference: "capture://build/1", Purpose: providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_BUILD}
	response := plan(t, planRequest(validInput(), obs, buildTarget(), buildRef))
	project := findAction(response, providerv0.ActionType_ACTION_TYPE_PROJECT_OUTPUT)
	if project == nil {
		t.Fatal("a supplied build credential must project error-tracking-build@1")
	}
	values := project.GetOutput().GetValues()
	if values["SENTRY_AUTH_TOKEN"].GetOpaqueReference().GetReference() != "capture://build/1" {
		t.Fatal("the build token must be projected as an opaque reference")
	}
	if values["SENTRY_ORG"].GetPublicValue().GetStringValue() != "acme" || values["SENTRY_PROJECT"].GetPublicValue().GetStringValue() != "web" {
		t.Fatal("org and project must be projected into the build contract")
	}
}

func TestPlanBuildOmittedWhenNoBuildCredential(t *testing.T) {
	obs := observation(projectResource(true, "acme", "web"))
	response := plan(t, planRequest(validInput(), obs, buildTarget()))
	if hasActionType(response, providerv0.ActionType_ACTION_TYPE_PROJECT_OUTPUT) {
		t.Fatal("without a build credential the optional build projection is omitted")
	}
	if !hasActionType(response, providerv0.ActionType_ACTION_TYPE_NO_OP) {
		t.Fatal("the build path with no credential is a NO_OP")
	}
}

// TestSetupTokenNeverProjected proves the setup credential never becomes output.
// Both a setup (management) reference and a build reference are supplied on a
// build plan; only the build reference may appear in the plan.
func TestSetupTokenNeverProjected(t *testing.T) {
	const setupRef = "secret://sentry-setup-token"
	obs := observation(projectResource(true, "acme", "web"))
	references := []*providerv0.OpaqueReference{
		{Reference: setupRef, Purpose: providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_MANAGEMENT},
		{Reference: "capture://build/1", Purpose: providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_BUILD},
	}
	response := plan(t, planRequest(validInput(), obs, buildTarget(), references...))
	encoded, err := proto.Marshal(response.GetPlan())
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if bytes.Contains(encoded, []byte(setupRef)) {
		t.Fatal("the setup credential reference must never appear in the plan")
	}
}

// TestRuntimeProjectionIsAllPublic proves the runtime error-tracking projection
// carries only public values — no opaque credential reference of any kind.
func TestRuntimeProjectionIsAllPublic(t *testing.T) {
	obs := observation(projectResource(true, "acme", "web"), clientKeyResource(keyA, true, dsnA))
	response := plan(t, planRequest(validInput(), obs, runtimeTarget()))
	project := findAction(response, providerv0.ActionType_ACTION_TYPE_PROJECT_OUTPUT)
	for name, value := range project.GetOutput().GetValues() {
		if value.GetOpaqueReference() != nil {
			t.Fatalf("runtime projection value %q must not be an opaque credential reference", name)
		}
	}
}

func TestSecondPlanIsDeterministic(t *testing.T) {
	obs := observation(projectResource(true, "acme", "web"), clientKeyResource(keyA, true, dsnA))
	first := plan(t, planRequest(validInput(), obs, runtimeTarget()))
	second := plan(t, planRequest(validInput(), obs, runtimeTarget()))
	if first.GetPlan().GetPlanDigest() != second.GetPlan().GetPlanDigest() {
		t.Fatal("a repeated plan over identical observation must produce an identical digest (no diff)")
	}
}

func TestPlanRejectsInvalidInput(t *testing.T) {
	input := validInput()
	delete(input, "organization_slug")
	response := plan(t, planRequest(input, observation(), runtimeTarget()))
	if !hasDiagnostic(response.GetDiagnostics(), DiagInvalidInput) {
		t.Fatal("invalid input must produce an invalid-input diagnostic")
	}
	if len(response.GetPlan().GetActions()) != 0 {
		t.Fatal("invalid input must not yield plan actions")
	}
}
