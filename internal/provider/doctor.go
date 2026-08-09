package provider

import (
	"context"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Doctor runs admitted read-only diagnostics: setup-credential reachability,
// project readiness (present, accessible, identity-matched), and client-key
// presence/ambiguity. It mutates nothing and reaches Sentry only through the
// same host-admitted reads Observe uses, so it is safe to run against
// production. It reports bounded, provider-neutral diagnostics — never a raw
// Sentry message.
func (s *Server) Doctor(ctx context.Context, request *providerv0.DoctorRequest) (*providerv0.DoctorResponse, error) {
	if s.host == nil {
		return nil, status.Error(codes.Unavailable, "provider host callback is not configured")
	}
	pctx := request.GetContext()
	in, err := parseInputs(pctx.GetOffline().GetInput())
	if err != nil {
		return &providerv0.DoctorResponse{
			Healthy:     false,
			Diagnostics: []*basev0.FailureDiagnostic{diag(basev0.FailureDiagnostic_ERROR, DiagInvalidInput, err.Error())},
		}, nil
	}
	if diagnostics := validateInputs(in); hasError(diagnostics) {
		return &providerv0.DoctorResponse{Healthy: false, Diagnostics: diagnostics}, nil
	}

	params := map[string]*providerv0.PublicValue{
		"organization_slug": publicStringValue(in.Organization),
		"project_slug":      publicStringValue(in.Project),
	}

	var diagnostics []*basev0.FailureDiagnostic
	healthy := true

	projectRead, err := s.read(ctx, pctx, requestProjectRetrieve, params, nil)
	if err != nil {
		return nil, err
	}
	if projectRead.ok {
		project := projectFromResponse(projectRead.fields)
		if project.Slug != in.Project || project.OrgSlug != in.Organization {
			healthy = false
			diagnostics = append(diagnostics, diag(basev0.FailureDiagnostic_ERROR, DiagProjectInaccessible,
				"the observed project is not the configured project"))
		}
	} else {
		healthy = false
		diagnostics = append(diagnostics, diag(doctorSeverity(projectRead.statusCode),
			ClassifySentryError(SentryError{StatusCode: int(projectRead.statusCode), RetryAfter: projectRead.statusCode == 429}),
			diagnosticMessageForStatus(projectRead.statusCode)))
		// The project could not be read; a client-key diagnostic on top of that
		// would be noise, so stop here.
		return &providerv0.DoctorResponse{Healthy: healthy, Diagnostics: diagnostics}, nil
	}

	keyRead, err := s.read(ctx, pctx, requestClientKeyList, params, nil)
	if err != nil {
		return nil, err
	}
	if !keyRead.ok {
		healthy = false
		diagnostics = append(diagnostics, diag(doctorSeverity(keyRead.statusCode),
			ClassifySentryError(SentryError{StatusCode: int(keyRead.statusCode), RetryAfter: keyRead.statusCode == 429}),
			diagnosticMessageForStatus(keyRead.statusCode)))
		return &providerv0.DoctorResponse{Healthy: healthy, Diagnostics: diagnostics}, nil
	}

	if diagnostic := clientKeyDiagnostic(clientKeysFromResponse(keyRead.fields), in.ClientKeyID); diagnostic != nil {
		healthy = false
		diagnostics = append(diagnostics, diagnostic)
	}

	return &providerv0.DoctorResponse{Healthy: healthy, Diagnostics: diagnostics}, nil
}

// clientKeyDiagnostic maps a client-key selection outcome to a diagnostic, or
// nil when a single active key resolves cleanly. It reuses the offline selection
// so Doctor and Plan agree on what a healthy key configuration is.
func clientKeyDiagnostic(keys []observedClientKey, explicitID string) *basev0.FailureDiagnostic {
	switch selectClientKey(keys, explicitID).Outcome {
	case selectionSelected:
		return nil
	case selectionNoActive:
		return diag(basev0.FailureDiagnostic_ERROR, DiagNoActiveKey, "no active client key exists for the configured project")
	case selectionAmbiguous:
		return diag(basev0.FailureDiagnostic_ERROR, DiagAmbiguousKey, "multiple active client keys exist; set client_key_id to select one")
	case selectionExplicitRevoked:
		return diag(basev0.FailureDiagnostic_ERROR, DiagKeyRevoked, "the configured client key is revoked")
	default: // selectionExplicitMissing
		return diag(basev0.FailureDiagnostic_ERROR, DiagKeyMismatch, "the configured client key is not owned by the configured project")
	}
}

// doctorSeverity treats a rate limit or server error as a transient warning
// rather than a hard failure: the credential may still be valid once the window
// clears. Authentication, permission, and not-found are definitive errors.
func doctorSeverity(statusCode uint32) basev0.FailureDiagnostic_Severity {
	if statusCode == 429 || statusCode >= 500 {
		return basev0.FailureDiagnostic_WARNING
	}
	return basev0.FailureDiagnostic_ERROR
}
