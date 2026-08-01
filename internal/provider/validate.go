package provider

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
)

var (
	slugPattern        = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	clientKeyIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
	projectIDPattern   = regexp.MustCompile(`^[0-9]+$`)
)

// Validate checks the normalized desired input offline, without broker access.
// It never contacts Sentry and never sees raw credentials.
func (s *Server) Validate(_ context.Context, request *providerv0.ValidateRequest) (*providerv0.ValidateResponse, error) {
	ctx := request.GetContext()
	in, err := parseInputs(ctx.GetInput())
	if err != nil {
		return &providerv0.ValidateResponse{
			Valid:       false,
			Diagnostics: []*basev0.FailureDiagnostic{diag(basev0.FailureDiagnostic_ERROR, DiagInvalidInput, err.Error())},
		}, nil
	}
	diagnostics := validateInputs(in)
	return &providerv0.ValidateResponse{Valid: !hasError(diagnostics), Diagnostics: diagnostics}, nil
}

func validateInputs(in inputs) []*basev0.FailureDiagnostic {
	var out []*basev0.FailureDiagnostic
	invalid := func(format string, args ...any) {
		out = append(out, diag(basev0.FailureDiagnostic_ERROR, DiagInvalidInput, fmt.Sprintf(format, args...)))
	}

	if in.Organization == "" {
		invalid("organization_slug is required")
	} else if !slugPattern.MatchString(in.Organization) {
		invalid("organization_slug %q is not a valid Sentry slug", in.Organization)
	}
	if in.Project == "" {
		invalid("project_slug is required")
	} else if !slugPattern.MatchString(in.Project) {
		invalid("project_slug %q is not a valid Sentry slug", in.Project)
	}

	switch in.Origin.Region {
	case regionInvalid:
		invalid("api_origin %q is not a usable https origin", in.Origin.Raw)
	case regionSelfHosted:
		// A self-hosted origin is admissible, but only through explicit,
		// per-deployment governed host admission — it is outside the SaaS
		// origin ceiling and may target a private network.
		out = append(out, diag(basev0.FailureDiagnostic_WARNING, DiagSelfHostedAdmission,
			fmt.Sprintf("api_origin %q is self-hosted; it requires explicit governed host admission and is never blanket-admitted", in.Origin.Host)))
	}

	if in.ClientKeyID != "" && !clientKeyIDPattern.MatchString(in.ClientKeyID) {
		invalid("client_key_id %q is not a Sentry client-key id", in.ClientKeyID)
	}

	if in.ExpectedDSN != "" {
		if err := validatePublicDSN(in.ExpectedDSN); err != nil {
			invalid("%s", err.Error())
		}
	}
	return out
}

// validatePublicDSN accepts only a public Sentry DSN: an https origin whose
// userinfo carries the public key and no password. A DSN that carries a
// password is a legacy secret DSN and must never be supplied as input.
func validatePublicDSN(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("expected_dsn is not a valid URL")
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("expected_dsn must be an https DSN")
	}
	if parsed.User == nil || parsed.User.Username() == "" {
		return fmt.Errorf("expected_dsn must carry a public key")
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		return fmt.Errorf("expected_dsn carries a secret; only the public DSN may be supplied")
	}
	projectID := strings.TrimPrefix(parsed.Path, "/")
	if !projectIDPattern.MatchString(projectID) {
		return fmt.Errorf("expected_dsn must end in a numeric project id")
	}
	return nil
}
