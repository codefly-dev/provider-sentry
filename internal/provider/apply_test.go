package provider

import (
	"context"
	"strings"
	"testing"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/configuration"
	"github.com/codefly-dev/core/provider/sdk"
	providerstate "github.com/codefly-dev/core/provider/state"
)

// projectOutputAction builds a bound PROJECT_OUTPUT action for the runtime
// error-tracking contract carrying the public DSN.
func projectOutputAction(t *testing.T) *providerv0.PlanAction {
	t.Helper()
	action, err := sdk.NewProjectOutputAction("error-tracking-project", 0, &providerv0.OutputProposal{
		Contract:         configuration.ErrorTrackingContract,
		TargetGeneration: 1,
		Values: map[string]*providerv0.OutputValue{
			"SENTRY_DSN": publicOutput("https://pub_a@sentry.io/11"),
		},
	})
	if err != nil {
		t.Fatalf("build project-output action: %v", err)
	}
	return action
}

func applyRequest(t *testing.T, action *providerv0.PlanAction) *providerv0.ApplyActionRequest {
	t.Helper()
	return &providerv0.ApplyActionRequest{
		Context: brokerContext(t, "op-apply", "act-apply", observeInput()),
		Plan:    &providerv0.OrderedPlan{PlanId: "plan-1", PlanDigest: "sha256:" + strings.Repeat("c", 64)},
		Action:  action,
	}
}

func TestApplyProjectOutputProjectsRuntimeDSN(t *testing.T) {
	host := newFakeHost(t)
	server := fakeServer(t, host)
	action := projectOutputAction(t)

	response, err := server.ApplyAction(context.Background(), applyRequest(t, action))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(host.outputs) != 1 {
		t.Fatalf("expected exactly one output proposed, got %d", len(host.outputs))
	}
	if host.outputs[0].GetContract() != configuration.ErrorTrackingContract {
		t.Fatalf("unexpected projected contract %q", host.outputs[0].GetContract())
	}
	state := response.GetNextState().GetV1()
	if state.GetOutputContract() != configuration.ErrorTrackingContract || state.GetOutputGeneration() != 1 {
		t.Fatalf("output not recorded in next state: %+v", state)
	}
	if err := providerstate.Validate(state, manifestStateSchemaVersion); err != nil {
		t.Fatalf("next state is invalid: %v", err)
	}
}

func TestApplyProjectOutputBuildToken(t *testing.T) {
	host := newFakeHost(t)
	server := fakeServer(t, host)
	proposal := &providerv0.OutputProposal{
		Contract:         configuration.ErrorTrackingBuildContract,
		TargetGeneration: 1,
		Values: map[string]*providerv0.OutputValue{
			"SENTRY_AUTH_TOKEN": referenceOutput(&providerv0.OpaqueReference{
				Reference: "capture://build", Purpose: providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_BUILD,
				SafeFingerprint: "sha256:" + strings.Repeat("d", 64),
			}),
			"SENTRY_ORG":     publicOutput("acme"),
			"SENTRY_PROJECT": publicOutput("web"),
		},
	}
	action, err := sdk.NewProjectOutputAction("error-tracking-build-project", 0, proposal)
	if err != nil {
		t.Fatalf("build action: %v", err)
	}
	if _, err := server.ApplyAction(context.Background(), applyRequest(t, action)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(host.outputs) != 1 || host.outputs[0].GetContract() != configuration.ErrorTrackingBuildContract {
		t.Fatalf("expected the build contract to be projected, got %+v", host.outputs)
	}
}

// TestApplyRefusesEveryMutation asserts the read-only invariant: no mutating
// action type can be applied, because this provider advertises no remote write.
func TestApplyRefusesEveryMutation(t *testing.T) {
	host := newFakeHost(t)
	server := fakeServer(t, host)
	for _, actionType := range []providerv0.ActionType{
		providerv0.ActionType_ACTION_TYPE_CREATE,
		providerv0.ActionType_ACTION_TYPE_UPDATE,
		providerv0.ActionType_ACTION_TYPE_REPLACE,
		providerv0.ActionType_ACTION_TYPE_DELETE,
		providerv0.ActionType_ACTION_TYPE_IMPORT,
	} {
		action := &providerv0.PlanAction{
			ActionId: "act-mut", Type: actionType, ResourceType: resourceClientKey,
			ProspectiveRemoteId: "x", RemoteIdentity: &providerv0.RemoteIdentity{Provider: "sentry", ResourceType: resourceClientKey, RemoteId: "x"},
		}
		if _, err := server.ApplyAction(context.Background(), applyRequest(t, action)); err == nil {
			t.Fatalf("mutating action %s must be refused", actionType)
		}
	}
	if len(host.outputs) != 0 {
		t.Fatal("a refused mutation must never reach the host")
	}
}

func TestApplyInertRecordsNoOp(t *testing.T) {
	host := newFakeHost(t)
	server := fakeServer(t, host)
	action := &providerv0.PlanAction{
		ActionId: "act-noop", Type: providerv0.ActionType_ACTION_TYPE_NO_OP,
		ResourceType: resourceClientKey, Ownership: providerv0.Ownership_OWNERSHIP_OBSERVED,
	}
	response, err := server.ApplyAction(context.Background(), applyRequest(t, action))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if response.GetReceipt().GetDelivery() != providerv0.DeliveryState_DELIVERY_STATE_NOT_SENT {
		t.Fatal("an inert action never crosses the network")
	}
	if err := providerstate.Validate(response.GetNextState().GetV1(), manifestStateSchemaVersion); err != nil {
		t.Fatalf("inert next state is invalid: %v", err)
	}
}

func TestApplyWithoutHostFailsClosed(t *testing.T) {
	server := testServer(t) // no host attached
	_, err := server.ApplyAction(context.Background(), applyRequest(t, projectOutputAction(t)))
	if err == nil {
		t.Fatal("apply must fail closed without a host callback channel")
	}
}
