package provider

import (
	"context"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	providerstate "github.com/codefly-dev/core/provider/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ApplyAction applies exactly one host-selected action from the bound plan. This
// provider advertises no remote mutation — no resource type carries a
// create/update/replace/delete/import action — so the only external effect it
// can apply is PROJECT_OUTPUT, driven through the host's ProposeOutput callback.
// The read-only invariant is asserted here, not assumed: a mutating action is
// refused rather than attempted, so an admitted request can never express a
// remote write.
func (s *Server) ApplyAction(ctx context.Context, request *providerv0.ApplyActionRequest) (*providerv0.ApplyActionResponse, error) {
	if s.host == nil {
		return nil, status.Error(codes.FailedPrecondition, "provider host callback channel is not attached")
	}
	action := request.GetAction()
	if action == nil {
		return nil, status.Error(codes.InvalidArgument, "apply action requires an action")
	}
	switch action.GetType() {
	case providerv0.ActionType_ACTION_TYPE_PROJECT_OUTPUT:
		return s.applyProjectOutput(ctx, request)
	case providerv0.ActionType_ACTION_TYPE_NO_OP, providerv0.ActionType_ACTION_TYPE_BLOCKED,
		providerv0.ActionType_ACTION_TYPE_MANUAL:
		return s.applyInert(request)
	case providerv0.ActionType_ACTION_TYPE_CREATE, providerv0.ActionType_ACTION_TYPE_UPDATE,
		providerv0.ActionType_ACTION_TYPE_REPLACE, providerv0.ActionType_ACTION_TYPE_DELETE,
		providerv0.ActionType_ACTION_TYPE_IMPORT:
		// The manifest declares no mutating action on any resource type, so a
		// mutating action can never have been planned. Assert the invariant rather
		// than attempt a write the provider has no request to perform.
		return nil, status.Errorf(codes.FailedPrecondition,
			"%s: this provider advertises no remote mutation; %s cannot be applied", DiagValidation, action.GetType())
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unsupported action type %s", action.GetType())
	}
}

// applyProjectOutput drives the host-selected output projection through
// ProposeOutput. The proposal is the exact bound proposal Plan produced — the
// runtime/browser error-tracking DSN (public only) or the build-only opaque
// build-token reference — and the setup token is never among its values.
func (s *Server) applyProjectOutput(ctx context.Context, request *providerv0.ApplyActionRequest) (*providerv0.ApplyActionResponse, error) {
	pctx := request.GetContext()
	proposal := request.GetAction().GetOutput()
	if proposal == nil {
		return nil, status.Error(codes.FailedPrecondition, DiagValidation+": project-output action carries no proposal")
	}
	response, err := s.host.ProposeOutput(ctx, &providerv0.ProposeOutputRequest{
		Operation: pctx.GetOperation(),
		Proposal:  proposal,
	})
	if err != nil {
		return nil, err
	}
	if !response.GetDurable() {
		return nil, status.Error(codes.FailedPrecondition, DiagValidation+": the host did not commit the output projection")
	}
	state := s.baseState(request, providerv0.Ownership_OWNERSHIP_OBSERVED)
	state.GetV1().OutputContract = proposal.GetContract()
	state.GetV1().OutputGeneration = response.GetGeneration()
	state.GetV1().OutputDigest = response.GetDigest()
	return &providerv0.ApplyActionResponse{Receipt: s.receipt(request), NextState: state}, nil
}

// applyInert records a no-op, blocked, or manual action without any broker call.
// The binding keeps exactly the state it entered with, or an owning-nothing base
// state when there was none.
func (s *Server) applyInert(request *providerv0.ApplyActionRequest) (*providerv0.ApplyActionResponse, error) {
	state := request.GetState()
	if state.GetV1() == nil {
		state = s.baseState(request, providerv0.Ownership_OWNERSHIP_OBSERVED)
	}
	return &providerv0.ApplyActionResponse{Receipt: s.receipt(request), NextState: state}, nil
}

// baseState builds the identity-complete provider state every action result
// carries. This provider owns no remote object, so state ownership is always
// observed.
func (s *Server) baseState(request *providerv0.ApplyActionRequest, ownership providerv0.Ownership) *providerv0.ProviderState {
	v1 := &providerv0.ProviderStateV1{
		StateSchemaVersion:    manifestStateSchemaVersion,
		Generation:            request.GetState().GetV1().GetGeneration() + 1,
		Binding:               request.GetContext().GetOffline().GetBinding(),
		ProviderId:            s.manifest.Agent.Publisher + "/" + s.manifest.Agent.Name,
		ProviderVersion:       s.manifest.Agent.Version,
		ManifestSchemaVersion: s.manifest.SchemaVersion,
		ManifestDigest:        s.manifestDigest,
		ArtifactDigest:        s.artifactDigest,
		Ownership:             ownership,
		PlanDigest:            request.GetPlan().GetPlanDigest(),
		Operation:             request.GetContext().GetOperation(),
	}
	return providerstate.WrapV1(v1)
}

// receipt records the durable action result. Every applied action here is a
// projection or an inert record — none crosses the network — so the receipt is a
// completed effect that was never sent, not an uncertain one.
func (s *Server) receipt(request *providerv0.ApplyActionRequest) *providerv0.ActionReceipt {
	operation := request.GetContext().GetOperation()
	return &providerv0.ActionReceipt{
		ReceiptId:      "receipt-" + operation.GetOperationId() + "-" + operation.GetActionId(),
		Operation:      operation,
		Action:         request.GetAction(),
		Delivery:       providerv0.DeliveryState_DELIVERY_STATE_NOT_SENT,
		Certainty:      providerv0.OutcomeCertainty_OUTCOME_CERTAINTY_COMPLETE,
		ArtifactDigest: s.artifactDigest,
	}
}
