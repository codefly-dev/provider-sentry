package provider

import (
	"fmt"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/manifest"
)

// Diagnostic codes advertised by the runtime catalog. Every code is strictly
// prefixed by the manifest diagnostic namespace and maps a bounded Sentry
// failure or planning class; no raw Sentry error body is ever surfaced through
// them.
const (
	diagNamespace = "provider.sentry."

	DiagInvalidInput      = diagNamespace + "invalid-input"
	DiagAuthentication    = diagNamespace + "authentication"
	DiagPermission        = diagNamespace + "permission"
	DiagNotFound          = diagNamespace + "not-found"
	DiagProjectInaccessible = diagNamespace + "project-inaccessible"
	DiagNoActiveKey       = diagNamespace + "no-active-client-key"
	DiagAmbiguousKey      = diagNamespace + "ambiguous-client-key"
	DiagKeyMismatch       = diagNamespace + "client-key-mismatch"
	DiagKeyRevoked        = diagNamespace + "client-key-revoked"
	DiagDSNMismatch       = diagNamespace + "supplied-dsn-mismatch"
	DiagSelfHostedAdmission = diagNamespace + "self-hosted-admission"
	DiagWrongRegion       = diagNamespace + "wrong-region"
	DiagRateLimit         = diagNamespace + "rate-limit"
	DiagScopeUnverified   = diagNamespace + "scope-unverified"
	DiagSchemaDrift       = diagNamespace + "schema-drift"
	DiagValidation        = diagNamespace + "permanent-validation"
	DiagTimeoutBeforeSend = diagNamespace + "timeout-before-send"
	DiagOutcomeUnknown    = diagNamespace + "outcome-unknown"
)

// diagnosticCodes is the exact, ordered set of codes the runtime advertises. It
// must remain a subset of the packaged manifest namespace.
var diagnosticCodes = []string{
	DiagInvalidInput, DiagAuthentication, DiagPermission, DiagNotFound,
	DiagProjectInaccessible, DiagNoActiveKey, DiagAmbiguousKey, DiagKeyMismatch,
	DiagKeyRevoked, DiagDSNMismatch, DiagSelfHostedAdmission, DiagWrongRegion,
	DiagRateLimit, DiagScopeUnverified, DiagSchemaDrift, DiagValidation,
	DiagTimeoutBeforeSend, DiagOutcomeUnknown,
}

// buildCatalog derives the runtime catalog from the packaged manifest. The
// catalog advertises the full manifest surface (a valid subset of itself) and
// is digest-bound so the host rejects any binary/manifest mismatch before
// admitting a request.
func buildCatalog(m *manifest.Manifest) (*providerv0.RuntimeCatalog, error) {
	local := &manifest.Catalog{
		SchemaVersion:       m.SchemaVersion,
		ProtocolVersion:     m.ProtocolVersion,
		StateSchemaVersions: append([]uint32(nil), m.StateSchemaVersions...),
		ProjectionContracts: make([]string, 0, len(m.Projections)),
		DiagnosticCodes:     append([]string(nil), diagnosticCodes...),
	}
	runtime := &providerv0.RuntimeCatalog{
		ProtocolVersion:       m.ProtocolVersion,
		ManifestSchemaVersion: m.SchemaVersion,
		StateSchemaVersions:   append([]uint32(nil), m.StateSchemaVersions...),
		ProjectionContracts:   make([]string, 0, len(m.Projections)),
		DiagnosticCodes:       append([]string(nil), diagnosticCodes...),
	}
	for _, descriptor := range m.Requests {
		d, err := manifest.RequestDescriptorDigest(descriptor)
		if err != nil {
			return nil, err
		}
		local.Requests = append(local.Requests, manifest.CatalogRequest{ID: descriptor.ID, Digest: d})
		runtime.Requests = append(runtime.Requests, &providerv0.RuntimeCatalogRequest{Id: descriptor.ID, Digest: d})
	}
	for _, resourceType := range m.ResourceTypes {
		actions := append([]string(nil), resourceType.Actions...)
		local.ResourceTypes = append(local.ResourceTypes, manifest.CatalogResource{ID: resourceType.ID, Actions: actions})
		runtime.ResourceTypes = append(runtime.ResourceTypes, &providerv0.RuntimeCatalogResource{Id: resourceType.ID, Actions: actions})
	}
	for _, projection := range m.Projections {
		local.ProjectionContracts = append(local.ProjectionContracts, projection.Contract)
		runtime.ProjectionContracts = append(runtime.ProjectionContracts, projection.Contract)
	}

	// Bind the runtime digest to the local catalog digest so the host's
	// AdmitRuntimeCatalog (which recomputes it) accepts exactly this surface.
	digest, err := local.Digest()
	if err != nil {
		return nil, err
	}
	runtime.Digest = digest

	// Fail closed if the derived catalog is not admissible against the manifest.
	if _, err := m.AdmitRuntimeCatalog(runtime); err != nil {
		return nil, fmt.Errorf("derived runtime catalog is not admissible: %w", err)
	}
	return runtime, nil
}
