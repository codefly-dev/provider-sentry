package provider

import (
	"os"
	"strings"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/configuration"
	"github.com/codefly-dev/core/provider/manifest"
)

const manifestPath = "../../provider.codefly.yaml"

func loadManifestBytes(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	return data
}

// testServer builds a server with an identity that matches the packaged
// manifest, using a syntactically valid placeholder artifact digest.
func testServer(t *testing.T) *Server {
	t.Helper()
	data := loadManifestBytes(t)
	m, err := manifest.Load(data)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	manifestDigest, err := m.Digest()
	if err != nil {
		t.Fatalf("manifest digest: %v", err)
	}
	server, err := NewServer(data, Identity{
		Publisher:      m.Agent.Publisher,
		Name:           m.Agent.Name,
		Version:        m.Agent.Version,
		ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
		ManifestDigest: manifestDigest,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return server
}

func binding() *providerv0.BindingAddress {
	return &providerv0.BindingAddress{WorkspaceId: "ws", EnvironmentId: "env", BindingId: "bind"}
}

func str(s string) *providerv0.PublicValue {
	return &providerv0.PublicValue{Kind: &providerv0.PublicValue_StringValue{StringValue: s}}
}

func boolVal(b bool) *providerv0.PublicValue {
	return &providerv0.PublicValue{Kind: &providerv0.PublicValue_BoolValue{BoolValue: b}}
}

// validInput is a well-formed desired input on the default SaaS origin.
func validInput() map[string]*providerv0.PublicValue {
	return map[string]*providerv0.PublicValue{
		"api_origin":        str("https://sentry.io"),
		"organization_slug": str("acme"),
		"project_slug":      str("web"),
		"environment":       str("production"),
	}
}

// clientKeyResource builds one observed client-key resource.
func clientKeyResource(id string, active bool, publicDSN string) *providerv0.MaterialResourceObservation {
	return &providerv0.MaterialResourceObservation{
		Identity:  &providerv0.RemoteIdentity{Provider: "sentry", ResourceType: resourceClientKey, RemoteId: id},
		Ownership: providerv0.Ownership_OWNERSHIP_OBSERVED,
		ProviderOwnedFields: map[string]*providerv0.PublicValue{
			fieldActive:    boolVal(active),
			fieldPublicDSN: str(publicDSN),
		},
	}
}

// projectResource builds one observed sentry.project resource.
func projectResource(accessible bool, org, slug string) *providerv0.MaterialResourceObservation {
	return &providerv0.MaterialResourceObservation{
		Identity:  &providerv0.RemoteIdentity{Provider: "sentry", ResourceType: resourceProject, RemoteId: "1"},
		Ownership: providerv0.Ownership_OWNERSHIP_OBSERVED,
		ProviderOwnedFields: map[string]*providerv0.PublicValue{
			fieldSlug:       str(slug),
			fieldOrgSlug:    str(org),
			fieldStatus:     str("active"),
			fieldAccessible: boolVal(accessible),
		},
	}
}

func observation(resources ...*providerv0.MaterialResourceObservation) *providerv0.MaterialObservation {
	return &providerv0.MaterialObservation{Complete: true, Resources: resources}
}

func runtimeTarget() *providerv0.OutputTarget {
	return &providerv0.OutputTarget{Contract: configuration.ErrorTrackingContract, TargetGeneration: 1}
}

func buildTarget() *providerv0.OutputTarget {
	return &providerv0.OutputTarget{Contract: configuration.ErrorTrackingBuildContract, TargetGeneration: 1}
}

func planRequest(input map[string]*providerv0.PublicValue, obs *providerv0.MaterialObservation, target *providerv0.OutputTarget, references ...*providerv0.OpaqueReference) *providerv0.PlanRequest {
	return &providerv0.PlanRequest{
		Context: &providerv0.OfflineProviderContext{
			Binding: binding(), Mode: providerv0.HostMode_HOST_MODE_DEVELOPMENT, AccountIdentity: "acme",
		},
		Desired:      &providerv0.BindingDesiredState{Binding: binding(), Input: input, CredentialReferences: references},
		Observation:  obs,
		OutputTarget: target,
		StateGeneration: &providerv0.StateGeneration{Generation: 1},
	}
}

func plan(t *testing.T, request *providerv0.PlanRequest) *providerv0.PlanResponse {
	t.Helper()
	response, err := testServer(t).Plan(t.Context(), request)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return response
}

func actionTypes(response *providerv0.PlanResponse) []providerv0.ActionType {
	var out []providerv0.ActionType
	for _, a := range response.GetPlan().GetActions() {
		out = append(out, a.GetType())
	}
	return out
}

func hasActionType(response *providerv0.PlanResponse, want providerv0.ActionType) bool {
	for _, a := range actionTypes(response) {
		if a == want {
			return true
		}
	}
	return false
}

func hasDiagnostic(diagnostics []*basev0.FailureDiagnostic, code string) bool {
	for _, d := range diagnostics {
		if d.GetCode() == code {
			return true
		}
	}
	return false
}
