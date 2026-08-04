package provider

import (
	"context"
	"fmt"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
)

// recordCheckpoint drives the host's durable pre-send checkpoint and fails
// closed unless the host confirms it committed. The broker refuses to send until
// a checkpoint binding this operation is durable; a read-only observation still
// records one so the pre-send ordering is identical to a mutation.
func (s *Server) recordCheckpoint(ctx context.Context, operation *providerv0.OperationIdentity, label string) error {
	checkpoint := &providerv0.ActionCheckpoint{
		CheckpointId: "checkpoint-" + operation.GetOperationId() + "-" + label,
		Operation:    operation,
		Delivery:     providerv0.DeliveryState_DELIVERY_STATE_NOT_SENT,
	}
	response, err := s.host.RecordCheckpoint(ctx, &providerv0.RecordCheckpointRequest{Checkpoint: checkpoint})
	if err != nil {
		return err
	}
	if !response.GetDurable() {
		return fmt.Errorf("host did not durably record the pre-send checkpoint")
	}
	return nil
}

// execute drives one host-admitted broker request. The provider supplies the
// planned request, the host-attested origin, and the purpose-matched handles;
// the host owns delivery, filtering, and suppression.
func (s *Server) execute(ctx context.Context, pctx *providerv0.ProviderContext, origin *providerv0.AdmittedOrigin, planned *providerv0.PlannedRequest, requestID string) (*providerv0.ExecuteRequestResponse, error) {
	handles, err := credentialHandles(pctx, planned.GetCredentialPurposes())
	if err != nil {
		return nil, err
	}
	return s.host.ExecuteRequest(ctx, &providerv0.ExecuteRequestRequest{
		Context:           pctx,
		RequestId:         requestID,
		Request:           planned,
		Origin:            origin,
		CredentialHandles: handles,
	})
}

// diagnoseResponse classifies a broker response into a blocking or advisory
// diagnostic, or nil when a success body was received. Sentry error bodies are
// dropped by the host, so a failure is classified by delivery and HTTP status
// alone — never by an untrusted, secret-shaped error body.
func diagnoseResponse(response *providerv0.ExecuteRequestResponse) *basev0.FailureDiagnostic {
	switch response.GetDelivery() {
	case providerv0.DeliveryState_DELIVERY_STATE_RESPONSE_RECEIVED:
		status := int(response.GetStatusCode())
		if status >= 200 && status < 300 {
			return nil
		}
		code := ClassifySentryError(SentryError{StatusCode: status})
		return diag(basev0.FailureDiagnostic_ERROR, code, fmt.Sprintf("Sentry responded with HTTP %d", status))
	case providerv0.DeliveryState_DELIVERY_STATE_NOT_SENT:
		return diag(basev0.FailureDiagnostic_ERROR, DiagTimeoutBeforeSend, "the request was not sent to Sentry")
	case providerv0.DeliveryState_DELIVERY_STATE_SENT_OUTCOME_UNKNOWN:
		return diag(basev0.FailureDiagnostic_WARNING, DiagOutcomeUnknown, "the request reached Sentry but its outcome is unknown")
	default:
		return diag(basev0.FailureDiagnostic_ERROR, DiagValidation, "the broker returned an unknown delivery state")
	}
}
