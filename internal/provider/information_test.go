package provider

import (
	"slices"
	"strings"
	"testing"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/configuration"
	"github.com/codefly-dev/core/provider/manifest"
)

func TestManifestValidatesAndCatalogIsAdmissible(t *testing.T) {
	m, err := manifest.Load(loadManifestBytes(t))
	if err != nil {
		t.Fatalf("manifest is invalid: %v", err)
	}
	catalog, err := buildCatalog(m)
	if err != nil {
		t.Fatalf("catalog build: %v", err)
	}
	if _, err := m.AdmitRuntimeCatalog(catalog); err != nil {
		t.Fatalf("catalog is not admissible against manifest: %v", err)
	}
	for _, want := range []string{configuration.ErrorTrackingContract, configuration.ErrorTrackingBuildContract} {
		if !slices.Contains(catalog.GetProjectionContracts(), want) {
			t.Fatalf("catalog does not project %q", want)
		}
	}
}

// TestManifestDeclaresNoMutation is the load-bearing invariant: an observe/
// project provider must not be able to express a remote mutation. No resource
// type may advertise create/update/replace/delete/import, and no request may be
// non-read-only.
func TestManifestDeclaresNoMutation(t *testing.T) {
	m, err := manifest.Load(loadManifestBytes(t))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	mutating := []string{"create", "update", "replace", "delete", "import"}
	for _, resourceType := range m.ResourceTypes {
		for _, action := range resourceType.Actions {
			if slices.Contains(mutating, action) {
				t.Fatalf("resource_type %q advertises mutating action %q", resourceType.ID, action)
			}
		}
	}
	for _, request := range m.Requests {
		if !request.ReadOnly || request.Method != "GET" {
			t.Fatalf("request %q is not read-only", request.ID)
		}
	}
}

func TestGetProviderInformationIsObserveOnly(t *testing.T) {
	server := testServer(t)
	response, err := server.GetProviderInformation(t.Context(), &providerv0.GetProviderInformationRequest{
		Artifact: &providerv0.AgentArtifactIdentity{
			Publisher:      "codefly.dev",
			Name:           "sentry",
			Version:        "0.1.0",
			ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
			ManifestDigest: server.manifestDigest,
		},
	})
	if err != nil {
		t.Fatalf("information: %v", err)
	}
	if response.GetCatalog().GetDigest() != server.catalogDigest {
		t.Fatal("catalog digest mismatch")
	}
	capabilities := response.GetCapabilities()
	if capabilities.GetSupportsImport() || capabilities.GetSupportsReplace() || capabilities.GetSupportsDelete() || capabilities.GetSupportsStateUpgrade() {
		t.Fatal("an observe/project provider must advertise no mutation capability")
	}
	if !response.GetReadiness().GetProductionObserve() {
		t.Fatal("the provider observes production")
	}
	if response.GetReadiness().GetProductionMutation() {
		t.Fatal("the provider must never advertise production mutation readiness")
	}
}

func TestNewServerRejectsManifestDigestMismatch(t *testing.T) {
	_, err := NewServer(loadManifestBytes(t), Identity{
		Publisher: "codefly.dev", Name: "sentry", Version: "0.1.0",
		ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
		ManifestDigest: "sha256:" + strings.Repeat("b", 64),
	})
	if err == nil {
		t.Fatal("expected manifest digest mismatch to fail closed")
	}
}
