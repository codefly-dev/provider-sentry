package provider

import (
	"bytes"
	"strings"
	"testing"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/configuration"
	providerstate "github.com/codefly-dev/core/provider/state"
	"google.golang.org/protobuf/proto"
)

// projectOutputAction runs the offline Plan and returns the single PROJECT_OUTPUT
// action it produces, so ApplyAction is exercised against a real plan-bound
// proposal rather than a hand-built one.
func projectOutputAction(t *testing.T, target *providerv0.OutputTarget, references ...*providerv0.OpaqueReference) (*providerv0.OrderedPlan, *providerv0.PlanAction) {
	t.Helper()
	obs := observation(projectResource(true, "acme", "web"), clientKeyResource(keyA, true, dsnA))
	response := plan(t, planRequest(validInput(), obs, target, references...))
	action := findAction(response, providerv0.ActionType_ACTION_TYPE_PROJECT_OUTPUT)
	if action == nil {
		t.Fatal("expected a PROJECT_OUTPUT action to apply")
	}
	return response.GetPlan(), action
}

func applyRequest(plan *providerv0.OrderedPlan, action *providerv0.PlanAction) *providerv0.ApplyActionRequest {
	return &providerv0.ApplyActionRequest{
		Context: hostContext(validInput()),
		Plan:    plan,
		Action:  action,
	}
}

// assertNoSetupToken fails if the opaque setup handle reaches a projection. The
// setup credential (project:read) is never an output value.
func assertNoSetupToken(t *testing.T, proposal *providerv0.OutputProposal) {
	t.Helper()
	raw, err := proto.Marshal(proposal)
	if err != nil {
		t.Fatalf("marshal proposal: %v", err)
	}
	if bytes.Contains(raw, []byte("setup-handle")) {
		t.Fatal("the setup credential must never be projected")
	}
	for name, value := range proposal.GetValues() {
		if reference := value.GetOpaqueReference(); reference != nil && reference.GetPurpose() == providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_MANAGEMENT {
			t.Fatalf("output %q projects a management credential", name)
		}
	}
}

func TestApplyProjectsRuntimeDSN(t *testing.T) {
	plan, action := projectOutputAction(t, runtimeTarget())
	host := newFakeHost(loadManifest(t))
	server := hostServer(t, host)

	response, err := server.ApplyAction(t.Context(), applyRequest(plan, action))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if response.GetReceipt() == nil {
		t.Fatal("apply must return a receipt")
	}
	if len(host.proposals) != 1 {
		t.Fatalf("expected exactly one projection proposed, got %d", len(host.proposals))
	}
	proposal := host.proposals[0].GetProposal()
	if proposal.GetContract() != configuration.ErrorTrackingContract {
		t.Fatalf("projected contract = %q", proposal.GetContract())
	}
	if dsn := proposal.GetValues()["SENTRY_DSN"].GetPublicValue().GetStringValue(); dsn != dsnA {
		t.Fatalf("projected DSN = %q, want %q", dsn, dsnA)
	}
	assertNoSetupToken(t, proposal)
}

func TestApplyProjectsBuildToken(t *testing.T) {
	buildRef := &providerv0.OpaqueReference{Reference: "capture://build/1", Purpose: providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_BUILD}
	plan, action := projectOutputAction(t, buildTarget(), buildRef)
	host := newFakeHost(loadManifest(t))
	server := hostServer(t, host)

	if _, err := server.ApplyAction(t.Context(), applyRequest(plan, action)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	proposal := host.proposals[0].GetProposal()
	if proposal.GetContract() != configuration.ErrorTrackingBuildContract {
		t.Fatalf("projected contract = %q", proposal.GetContract())
	}
	if reference := proposal.GetValues()["SENTRY_AUTH_TOKEN"].GetOpaqueReference().GetReference(); reference != "capture://build/1" {
		t.Fatalf("build token reference = %q", reference)
	}
	assertNoSetupToken(t, proposal)
}

func TestApplyRejectsRemoteMutation(t *testing.T) {
	host := newFakeHost(loadManifest(t))
	server := hostServer(t, host)
	for _, actionType := range []providerv0.ActionType{
		providerv0.ActionType_ACTION_TYPE_CREATE,
		providerv0.ActionType_ACTION_TYPE_UPDATE,
		providerv0.ActionType_ACTION_TYPE_REPLACE,
		providerv0.ActionType_ACTION_TYPE_DELETE,
		providerv0.ActionType_ACTION_TYPE_IMPORT,
	} {
		action := &providerv0.PlanAction{ActionId: "a1", ResourceType: resourceClientKey, Type: actionType, ProspectiveRemoteId: keyA}
		if _, err := server.ApplyAction(t.Context(), applyRequest(nil, action)); err == nil {
			t.Fatalf("mutation action %s must be rejected", actionType)
		}
	}
	if len(host.proposals) != 0 {
		t.Fatal("a rejected mutation must make no host callback")
	}
}

func TestApplyNoOpIsInert(t *testing.T) {
	host := newFakeHost(loadManifest(t))
	server := hostServer(t, host)
	action := &providerv0.PlanAction{ActionId: "a1", ResourceType: resourceClientKey, Type: providerv0.ActionType_ACTION_TYPE_NO_OP}

	response, err := server.ApplyAction(t.Context(), applyRequest(nil, action))
	if err != nil {
		t.Fatalf("apply no-op: %v", err)
	}
	if response.GetReceipt() == nil {
		t.Fatal("a no-op must still return a receipt")
	}
	if len(host.proposals) != 0 {
		t.Fatal("a no-op must make no host callback")
	}
}

func TestApplyProjectionRequiresDurableCommit(t *testing.T) {
	plan, action := projectOutputAction(t, runtimeTarget())
	host := newFakeHost(loadManifest(t))
	host.proposeDurable = false
	server := hostServer(t, host)

	if _, err := server.ApplyAction(t.Context(), applyRequest(plan, action)); err == nil {
		t.Fatal("a projection the host did not commit must fail")
	}
}

func TestApplyRejectsProjectionNotInBoundPlan(t *testing.T) {
	// A well-formed PROJECT_OUTPUT action that the bound plan does not carry must
	// be refused before any callback — the provider applies only what its own
	// Plan authored, which is what keeps a forged proposal (e.g. one bearing the
	// setup token) from reaching the host.
	_, action := projectOutputAction(t, runtimeTarget())
	host := newFakeHost(loadManifest(t))
	server := hostServer(t, host)

	request := &providerv0.ApplyActionRequest{
		Context: hostContext(validInput()),
		Plan:    &providerv0.OrderedPlan{}, // does not contain the action
		Action:  action,
	}
	if _, err := server.ApplyAction(t.Context(), request); err == nil {
		t.Fatal("a projection action absent from the bound plan must be rejected")
	}
	if len(host.proposals) != 0 {
		t.Fatal("a rejected projection must make no host callback")
	}
}

func TestApplyStampsProjectionOntoPriorState(t *testing.T) {
	plan, action := projectOutputAction(t, runtimeTarget())
	host := newFakeHost(loadManifest(t))
	host.proposeGeneration = 7
	server := hostServer(t, host)

	prior := providerstate.WrapV1(&providerv0.ProviderStateV1{
		StateSchemaVersion:    1,
		Binding:               binding(),
		ProviderId:            "codefly.dev/sentry",
		ProviderVersion:       "0.1.0",
		ManifestSchemaVersion: "codefly.provider-manifest/v0",
		ManifestDigest:        "sha256:" + strings.Repeat("a", 64),
		ArtifactDigest:        "sha256:" + strings.Repeat("b", 64),
		Ownership:             providerv0.Ownership_OWNERSHIP_OBSERVED,
	})
	request := &providerv0.ApplyActionRequest{Context: hostContext(validInput()), Plan: plan, Action: action, State: prior}

	response, err := server.ApplyAction(t.Context(), request)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	v1 := response.GetNextState().GetV1()
	if v1.GetOutputGeneration() != 7 {
		t.Fatalf("committed output generation not recorded: %d", v1.GetOutputGeneration())
	}
	if v1.GetOutputContract() != configuration.ErrorTrackingContract {
		t.Fatalf("output contract not recorded: %q", v1.GetOutputContract())
	}
}

func TestApplyRecordsProjectionWithoutPriorState(t *testing.T) {
	// With no prior state threaded, the committed generation must still be
	// recorded on a freshly minted, valid state — never silently dropped.
	plan, action := projectOutputAction(t, runtimeTarget())
	host := newFakeHost(loadManifest(t))
	host.proposeGeneration = 3
	server := hostServer(t, host)

	response, err := server.ApplyAction(t.Context(), applyRequest(plan, action)) // no State
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	next := response.GetNextState()
	if next == nil {
		t.Fatal("a committed projection must be recorded even without a prior state")
	}
	if next.GetV1().GetOutputGeneration() != 3 {
		t.Fatalf("committed output generation not recorded: %d", next.GetV1().GetOutputGeneration())
	}
	if err := providerstate.Validate(next.GetV1(), 1); err != nil {
		t.Fatalf("minted next state must be valid: %v", err)
	}
}
