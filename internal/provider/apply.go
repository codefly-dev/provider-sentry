package provider

import (
	"context"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ApplyAction applies exactly one host-selected action from the bound plan. This
// provider advertises no remote mutation — no resource type carries
// create/update/replace/delete/import — so the only durable effect it can apply
// is a PROJECT_OUTPUT projection through the host's ProposeOutput callback. The
// read-only invariant is asserted here, not assumed: a mutating action type is
// refused before any callback, so an admitted request can never express a remote
// write even if a caller constructs one.
func (s *Server) ApplyAction(ctx context.Context, request *providerv0.ApplyActionRequest) (*providerv0.ApplyActionResponse, error) {
	action := request.GetAction()
	if action == nil {
		return nil, status.Error(codes.InvalidArgument, "apply action requires an action")
	}
	switch action.GetType() {
	case providerv0.ActionType_ACTION_TYPE_PROJECT_OUTPUT:
		return s.applyProjectOutput(ctx, request)
	case providerv0.ActionType_ACTION_TYPE_NO_OP:
		return s.applyInert(request), nil
	case providerv0.ActionType_ACTION_TYPE_CREATE,
		providerv0.ActionType_ACTION_TYPE_UPDATE,
		providerv0.ActionType_ACTION_TYPE_REPLACE,
		providerv0.ActionType_ACTION_TYPE_DELETE,
		providerv0.ActionType_ACTION_TYPE_IMPORT:
		return nil, status.Errorf(codes.FailedPrecondition,
			"provider-sentry performs no remote mutation; action type %s is not applicable", action.GetType())
	default:
		// BLOCKED and MANUAL are terminal planning outcomes the host resolves
		// out of band; they are never applied.
		return nil, status.Errorf(codes.FailedPrecondition,
			"action type %s is not applicable by this provider", action.GetType())
	}
}

// applyProjectOutput commits a projection through the host. The proposal is the
// exact one the offline Plan bound; the provider forwards it unchanged, so the
// setup token — never part of any proposal — cannot enter an output here.
func (s *Server) applyProjectOutput(ctx context.Context, request *providerv0.ApplyActionRequest) (*providerv0.ApplyActionResponse, error) {
	if s.host == nil {
		return nil, status.Error(codes.Unavailable, "provider host callback is not configured")
	}
	action := request.GetAction()
	proposal := action.GetOutput()
	if proposal == nil {
		return nil, status.Error(codes.InvalidArgument, "project output action requires an output proposal")
	}
	operation := request.GetContext().GetOperation()
	response, err := s.host.ProposeOutput(ctx, &providerv0.ProposeOutputRequest{
		Operation: operation,
		Proposal:  proposal,
	})
	if err != nil {
		return nil, err
	}
	if !response.GetDurable() {
		return nil, status.Error(codes.Unavailable, "the host did not commit the projection")
	}
	receipt := &providerv0.ActionReceipt{
		Operation:      operation,
		Action:         action,
		Delivery:       providerv0.DeliveryState_DELIVERY_STATE_NOT_SENT,
		Certainty:      providerv0.OutcomeCertainty_OUTCOME_CERTAINTY_COMPLETE,
		ArtifactDigest: s.artifactDigest,
	}
	return &providerv0.ApplyActionResponse{
		Receipt:   receipt,
		NextState: projectedState(request.GetState(), proposal, response),
	}, nil
}

// applyInert acknowledges a NO_OP action. It makes no callback and changes no
// state: the observe-only path proves the Plan/Apply/commit machinery works even
// when an action has no remote effect.
func (s *Server) applyInert(request *providerv0.ApplyActionRequest) *providerv0.ApplyActionResponse {
	action := request.GetAction()
	return &providerv0.ApplyActionResponse{
		Receipt: &providerv0.ActionReceipt{
			Operation:      request.GetContext().GetOperation(),
			Action:         action,
			Delivery:       providerv0.DeliveryState_DELIVERY_STATE_NOT_SENT,
			Certainty:      providerv0.OutcomeCertainty_OUTCOME_CERTAINTY_COMPLETE,
			ArtifactDigest: s.artifactDigest,
		},
		NextState: request.GetState(),
	}
}

// projectedState records the committed output generation on the provider state.
// State is otherwise unchanged: a projection mutates no remote object.
func projectedState(current *providerv0.ProviderState, proposal *providerv0.OutputProposal, response *providerv0.ProposeOutputResponse) *providerv0.ProviderState {
	if current == nil {
		return nil
	}
	next := proto.Clone(current).(*providerv0.ProviderState)
	if v1 := next.GetV1(); v1 != nil {
		v1.OutputContract = proposal.GetContract()
		v1.OutputGeneration = response.GetGeneration()
		v1.OutputDigest = response.GetDigest()
	}
	return next
}
