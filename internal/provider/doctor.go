package provider

import (
	"context"
	"strings"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Doctor runs admitted read-only diagnostics: it confirms the setup credential
// reaches Sentry and can read the organization, reports whether the configured
// project is present and active, and whether exactly one active client key is
// selectable. It performs no mutation and surfaces only neutral, bounded
// diagnostics — never a raw credential, a secret, or a Sentry error body.
func (s *Server) Doctor(ctx context.Context, request *providerv0.DoctorRequest) (*providerv0.DoctorResponse, error) {
	if s.host == nil {
		return nil, status.Error(codes.FailedPrecondition, "provider host callback channel is not attached")
	}
	pctx := request.GetContext()
	in, err := parseInputs(pctx.GetOffline().GetInput())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	origin, err := admittedOrigin(pctx, originRuleAPI)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if err := s.recordCheckpoint(ctx, pctx.GetOperation(), "doctor"); err != nil {
		return nil, err
	}

	var diagnostics []*basev0.FailureDiagnostic

	projectFields, diagnostic, err := s.doctorRead(ctx, pctx, origin, "project.list", map[string]*providerv0.PublicValue{"organization_slug": publicStringValue(in.Organization)}, nil, "doctor-projects")
	if err != nil {
		return nil, err
	}
	if diagnostic != nil {
		// The credential cannot reach Sentry or read the organization: not healthy,
		// but the diagnostic stays neutral and body-free.
		return &providerv0.DoctorResponse{Healthy: false, Diagnostics: []*basev0.FailureDiagnostic{diagnostic}}, nil
	}

	projectID := ""
	projectStatus := ""
	for _, index := range projectFields.rootIndices() {
		prefix := elementPath(index)
		if projectFields.string(prefix+".slug") == in.Project {
			projectID = projectFields.string(prefix + ".id")
			projectStatus = projectFields.string(prefix + ".status")
			break
		}
	}
	if projectID == "" {
		return &providerv0.DoctorResponse{
			Healthy:     false,
			Diagnostics: []*basev0.FailureDiagnostic{diag(basev0.FailureDiagnostic_ERROR, DiagProjectInaccessible, "the configured project is not readable with the setup credential")},
		}, nil
	}
	if projectStatus != "" && projectStatus != "active" {
		diagnostics = append(diagnostics, diag(basev0.FailureDiagnostic_WARNING, DiagValidation,
			"the configured project is reachable but not active"))
	}

	keyFields, diagnostic, err := s.doctorRead(ctx, pctx, origin, "client-key.list", map[string]*providerv0.PublicValue{"organization_slug": publicStringValue(in.Organization)}, nil, "doctor-client-keys")
	if err != nil {
		return nil, err
	}
	if diagnostic != nil {
		return &providerv0.DoctorResponse{Healthy: false, Diagnostics: []*basev0.FailureDiagnostic{diagnostic}}, nil
	}
	if diagnostic := doctorKeyDiagnostic(keyFields, projectID, in.ClientKeyID); diagnostic != nil {
		diagnostics = append(diagnostics, diagnostic)
	}

	return &providerv0.DoctorResponse{Healthy: !hasError(diagnostics), Diagnostics: diagnostics}, nil
}

// doctorRead executes one read-only diagnostic request and returns either its
// filtered fields or a blocking diagnostic. A transport-level error is a hard
// failure; a non-success delivery is a neutral diagnostic.
func (s *Server) doctorRead(ctx context.Context, pctx *providerv0.ProviderContext, origin *providerv0.AdmittedOrigin, descriptorID string, pathParams, query map[string]*providerv0.PublicValue, requestID string) (filteredResponse, *basev0.FailureDiagnostic, error) {
	planned, err := s.plannedRequest(descriptorID, origin, pathParams, query)
	if err != nil {
		return nil, nil, err
	}
	response, err := s.execute(ctx, pctx, origin, planned, requestID)
	if err != nil {
		return nil, nil, err
	}
	if diagnostic := diagnoseResponse(response); diagnostic != nil {
		return nil, diagnostic, nil
	}
	fields, err := decodeFiltered(response)
	if err != nil {
		return nil, nil, err
	}
	return fields, nil, nil
}

// doctorKeyDiagnostic reports the client-key readiness of the project: no active
// key is a manual action, multiple active keys are ambiguous, and an explicit
// key that is missing or revoked is blocked. Exactly one selectable key is
// healthy (nil).
func doctorKeyDiagnostic(fields filteredResponse, projectID, explicitID string) *basev0.FailureDiagnostic {
	var keys []observedClientKey
	for _, index := range fields.rootIndices() {
		prefix := elementPath(index)
		if fields.string(prefix+".projectId") != projectID {
			continue
		}
		keys = append(keys, observedClientKey{
			ID:        fields.string(prefix + ".id"),
			Active:    fields.bool(prefix + ".isActive"),
			PublicDSN: fields.string(prefix + ".dsn.public"),
		})
	}
	switch sel := selectClientKey(keys, explicitID); sel.Outcome {
	case selectionSelected:
		return nil
	case selectionNoActive:
		return diag(basev0.FailureDiagnostic_WARNING, DiagNoActiveKey, "the configured project has no active client key")
	case selectionAmbiguous:
		return diag(basev0.FailureDiagnostic_WARNING, DiagAmbiguousKey,
			"the configured project has multiple active client keys ("+strings.Join(sel.ActiveIDs, ", ")+"); set client_key_id")
	case selectionExplicitRevoked:
		return diag(basev0.FailureDiagnostic_ERROR, DiagKeyRevoked, "the configured client_key_id is revoked")
	default: // selectionExplicitMissing
		return diag(basev0.FailureDiagnostic_ERROR, DiagKeyMismatch, "the configured client_key_id is not a client key of this project")
	}
}
