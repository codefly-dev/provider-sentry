// Package provider implements the Sentry reference provider agent for Codefly.
//
// It speaks the codefly.provider/v0 protocol: the host drives it through the
// Provider service (GetProviderInformation, Validate, Observe, Plan,
// ApplyAction, Doctor, UpgradeState) and the provider reaches Sentry only
// through the host broker's ProviderHost callbacks. It is the observe/project
// counterexample to Stripe: it declares no mutating request, holds no Sentry
// management token, and never receives a client-key `secret` or `dsn.secret`.
// This package implements the offline surface (information, validate, plan,
// upgrade); the broker-driven surface (Observe, ApplyAction, Doctor) lands with
// the host runtime and cassettes.
package provider

import (
	"fmt"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/manifest"
	"github.com/codefly-dev/core/provider/sdk"
)

// Identity is the verified artifact identity of the running provider, taken
// from the installed provider.artifact.json descriptor. It binds the binary to
// its manifest and is echoed back to the host in GetProviderInformation.
type Identity struct {
	Publisher      string
	Name           string
	Version        string
	ArtifactDigest string
	ManifestDigest string
}

// Server implements providerv0.ProviderServer. The offline methods are
// implemented here; GetProviderInformation is served by the embedded sdk.Base,
// and the broker-driven methods (Observe, ApplyAction, Doctor) remain
// unimplemented until the host runtime is available.
type Server struct {
	*sdk.Base
	manifest       *manifest.Manifest
	catalog        *providerv0.RuntimeCatalog
	artifactDigest string
	manifestDigest string
	catalogDigest  string
}

var _ providerv0.ProviderServer = (*Server)(nil)

// NewServer builds a provider server from the packaged manifest bytes and the
// verified artifact identity. It fails closed when the identity does not match
// the packaged manifest, so a tampered manifest or mismatched descriptor can
// never be advertised as authentic.
func NewServer(manifestBytes []byte, id Identity) (*Server, error) {
	m, err := manifest.Load(manifestBytes)
	if err != nil {
		return nil, fmt.Errorf("packaged manifest is invalid: %w", err)
	}
	manifestDigest, err := m.Digest()
	if err != nil {
		return nil, err
	}
	if id.ManifestDigest != manifestDigest {
		return nil, fmt.Errorf("artifact manifest digest does not match packaged manifest")
	}
	if id.Publisher != m.Agent.Publisher || id.Name != m.Agent.Name || id.Version != m.Agent.Version {
		return nil, fmt.Errorf("artifact identity does not match packaged manifest agent")
	}
	catalog, err := buildCatalog(m)
	if err != nil {
		return nil, err
	}
	information := &providerv0.GetProviderInformationResponse{
		Artifact: &providerv0.AgentArtifactIdentity{
			Publisher:      id.Publisher,
			Name:           id.Name,
			Version:        id.Version,
			ArtifactDigest: id.ArtifactDigest,
			ManifestDigest: id.ManifestDigest,
		},
		Catalog: catalog,
		// v0.1 is observe/project only: it imports, replaces, and deletes
		// nothing, and defines a single state schema with no upgrade.
		Capabilities: &providerv0.ProviderCapabilities{
			SupportsImport:       false,
			SupportsReplace:      false,
			SupportsDelete:       false,
			SupportsStateUpgrade: false,
		},
		// The provider observes production safely and mutates nothing anywhere,
		// so production mutation readiness is structurally absent.
		Readiness: &providerv0.ProviderReadiness{
			ProductionObserve:  true,
			ProductionMutation: false,
		},
	}
	base, err := sdk.NewBase(information)
	if err != nil {
		return nil, err
	}
	return &Server{
		Base:           base,
		manifest:       m,
		catalog:        catalog,
		artifactDigest: id.ArtifactDigest,
		manifestDigest: id.ManifestDigest,
		catalogDigest:  catalog.GetDigest(),
	}, nil
}
