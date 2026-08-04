package provider

import (
	"context"
	"sort"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/sdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Observe reads the configured Sentry project and its client keys through the
// host broker and projects only the Codefly-owned safe fields the offline Plan
// consumes. It never sees a raw credential, a client-key `secret`, or a
// `dsn.secret`: the manifest suppresses both legacy secret locations before the
// broker forwards anything, so only the public DSN and safe identifiers reach
// this process.
//
// Both reads are organization-scoped because the host broker binds every request
// path to one remote-id segment (the organization slug): the provider lists the
// organization's projects and selects the configured one by slug, then lists the
// organization's client keys and keeps only those owned by that project.
func (s *Server) Observe(ctx context.Context, request *providerv0.ObserveRequest) (*providerv0.ObserveResponse, error) {
	if s.host == nil {
		return nil, status.Error(codes.FailedPrecondition, "provider host callback channel is not attached")
	}
	pctx := request.GetContext()
	offline := pctx.GetOffline()
	in, err := parseInputs(offline.GetInput())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	origin, err := admittedOrigin(pctx, originRuleAPI)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if err := s.recordCheckpoint(ctx, pctx.GetOperation(), "observe"); err != nil {
		return nil, err
	}

	projectID, projectResource, err := s.observeProject(ctx, pctx, origin, in)
	if err != nil {
		return nil, err
	}
	resources := []*providerv0.MaterialResourceObservation{projectResource}

	// A project the setup credential cannot see has no observable keys; the plan
	// gate blocks on the inaccessible project before any key selection.
	if projectID != "" {
		keys, err := s.observeClientKeys(ctx, pctx, origin, request.GetCursor(), in.Organization, projectID)
		if err != nil {
			return nil, err
		}
		resources = append(resources, keys...)
	}

	material := &providerv0.MaterialObservation{
		AccountIdentity: in.Organization,
		Mode:            offline.GetMode(),
		Complete:        true,
		Resources:       resources,
	}
	return sdk.Observation(material, nil)
}

// observeProject lists the organization's projects and selects the configured
// one by slug. The returned resource carries the provider's safe projection; a
// project the credential cannot see (absent from the list) is projected as
// inaccessible so the plan gate blocks rather than proceeds.
func (s *Server) observeProject(ctx context.Context, pctx *providerv0.ProviderContext, origin *providerv0.AdmittedOrigin, in inputs) (string, *providerv0.MaterialResourceObservation, error) {
	pathParams := map[string]*providerv0.PublicValue{"organization_slug": publicStringValue(in.Organization)}
	planned, err := s.plannedRequest("project.list", origin, pathParams, nil)
	if err != nil {
		return "", nil, err
	}
	response, err := s.execute(ctx, pctx, origin, planned, "observe-projects")
	if err != nil {
		return "", nil, err
	}
	if diagnostic := diagnoseResponse(response); diagnostic != nil {
		return "", nil, observeFailure(diagnostic)
	}
	fields, err := decodeFiltered(response)
	if err != nil {
		return "", nil, err
	}
	for _, index := range fields.rootIndices() {
		prefix := elementPath(index)
		if fields.string(prefix+".slug") != in.Project {
			continue
		}
		remoteID := fields.string(prefix + ".id")
		resource := &providerv0.MaterialResourceObservation{
			Identity:  remoteIdentity(resourceProject, remoteID, in.Organization),
			Ownership: providerv0.Ownership_OWNERSHIP_OBSERVED,
			ProviderOwnedFields: map[string]*providerv0.PublicValue{
				fieldSlug:       publicStringValue(fields.string(prefix + ".slug")),
				fieldName:       publicStringValue(fields.string(prefix + ".name")),
				fieldOrgSlug:    publicStringValue(in.Organization),
				fieldStatus:     publicStringValue(fields.string(prefix + ".status")),
				fieldAccessible: boolOutput(true),
			},
		}
		return remoteID, resource, nil
	}

	// The configured project is not among the organization's projects the setup
	// credential can read: project it as inaccessible so the plan gate blocks.
	inaccessible := &providerv0.MaterialResourceObservation{
		Identity:  remoteIdentity(resourceProject, "", in.Organization),
		Ownership: providerv0.Ownership_OWNERSHIP_OBSERVED,
		ProviderOwnedFields: map[string]*providerv0.PublicValue{
			fieldOrgSlug:    publicStringValue(in.Organization),
			fieldAccessible: boolOutput(false),
		},
	}
	return "", inaccessible, nil
}

// observeClientKeys lists the organization's client keys and keeps only those
// owned by the selected project. A host-supplied cursor continues an incomplete
// observation; the pinned broker forwards only body fields, so Sentry's
// Link-header pagination cursor is not visible to provider code and multi-page
// continuation is host-driven. The selection safety (never array[0], block on
// ambiguity or incompleteness) lives in Plan.
func (s *Server) observeClientKeys(ctx context.Context, pctx *providerv0.ProviderContext, origin *providerv0.AdmittedOrigin, cursor, org, projectID string) ([]*providerv0.MaterialResourceObservation, error) {
	query := map[string]*providerv0.PublicValue(nil)
	if cursor != "" {
		query = map[string]*providerv0.PublicValue{"cursor": publicStringValue(cursor)}
	}
	planned, err := s.plannedRequest("client-key.list", origin, map[string]*providerv0.PublicValue{"organization_slug": publicStringValue(org)}, query)
	if err != nil {
		return nil, err
	}
	response, err := s.execute(ctx, pctx, origin, planned, "observe-client-keys")
	if err != nil {
		return nil, err
	}
	if diagnostic := diagnoseResponse(response); diagnostic != nil {
		return nil, observeFailure(diagnostic)
	}
	fields, err := decodeFiltered(response)
	if err != nil {
		return nil, err
	}
	var resources []*providerv0.MaterialResourceObservation
	for _, index := range fields.rootIndices() {
		prefix := elementPath(index)
		// The organization-scoped list spans every project; keep only the keys
		// owned by the selected project so selection is over the right set.
		if fields.string(prefix+".projectId") != projectID {
			continue
		}
		resources = append(resources, &providerv0.MaterialResourceObservation{
			Identity:  remoteIdentity(resourceClientKey, fields.string(prefix+".id"), org),
			Ownership: providerv0.Ownership_OWNERSHIP_OBSERVED,
			ProviderOwnedFields: map[string]*providerv0.PublicValue{
				fieldActive:    boolOutput(fields.bool(prefix + ".isActive")),
				fieldPublicDSN: publicStringValue(fields.string(prefix + ".dsn.public")),
			},
		})
	}
	return resources, nil
}

// observeFailure turns a blocking response diagnostic into a fail-closed gRPC
// error. A read that cannot be trusted must never surface a partial, plausible
// observation the planner would act on.
func observeFailure(diagnostic *basev0.FailureDiagnostic) error {
	return status.Error(codes.FailedPrecondition, diagnostic.GetCode()+": "+diagnostic.GetMessage())
}

func boolOutput(value bool) *providerv0.PublicValue {
	return &providerv0.PublicValue{Kind: &providerv0.PublicValue_BoolValue{BoolValue: value}}
}

// Resource types and canonical observed-field keys. The provider owns these
// keys: a filtered Observe writes them and Plan reads them, so selection is over
// a stable, provider-defined projection rather than raw Sentry fields. A secret
// is never among them.
const (
	resourceProject   = "sentry.project"
	resourceClientKey = "sentry.client-key"

	fieldSlug       = "slug"
	fieldName       = "name"
	fieldStatus     = "status"
	fieldOrgSlug    = "organization_slug"
	fieldAccessible = "accessible"
	fieldActive     = "is_active"
	fieldPublicDSN  = "public_dsn"
)

// observedProject is the provider's projection of the remote Sentry project.
type observedProject struct {
	RemoteID   string
	Slug       string
	Name       string
	OrgSlug    string
	Status     string
	Accessible bool
}

// observedClientKey is the provider's projection of one Sentry client key. Only
// the safe id/status and the public DSN are present; no secret ever is.
type observedClientKey struct {
	ID        string
	Active    bool
	PublicDSN string
}

// observedProjectFrom extracts the single sentry.project projection, if present.
func observedProjectFrom(observation *providerv0.MaterialObservation) *observedProject {
	for _, resource := range observation.GetResources() {
		if resource.GetIdentity().GetResourceType() != resourceProject {
			continue
		}
		fields := resource.GetProviderOwnedFields()
		return &observedProject{
			RemoteID:   resource.GetIdentity().GetRemoteId(),
			Slug:       fields[fieldSlug].GetStringValue(),
			Name:       fields[fieldName].GetStringValue(),
			OrgSlug:    fields[fieldOrgSlug].GetStringValue(),
			Status:     fields[fieldStatus].GetStringValue(),
			Accessible: fields[fieldAccessible].GetBoolValue(),
		}
	}
	return nil
}

// observedClientKeys extracts the client-key projections in stable, cursor-order
// as observed (the host preserves list order across the complete pagination).
func observedClientKeys(observation *providerv0.MaterialObservation) []observedClientKey {
	var out []observedClientKey
	for _, resource := range observation.GetResources() {
		if resource.GetIdentity().GetResourceType() != resourceClientKey {
			continue
		}
		fields := resource.GetProviderOwnedFields()
		out = append(out, observedClientKey{
			ID:        resource.GetIdentity().GetRemoteId(),
			Active:    fields[fieldActive].GetBoolValue(),
			PublicDSN: fields[fieldPublicDSN].GetStringValue(),
		})
	}
	return out
}

// selectionOutcome is the deterministic result of client-key selection.
type selectionOutcome string

const (
	selectionSelected        selectionOutcome = "selected"
	selectionNoActive        selectionOutcome = "no-active"
	selectionAmbiguous       selectionOutcome = "ambiguous"
	selectionExplicitMissing selectionOutcome = "explicit-missing"
	selectionExplicitRevoked selectionOutcome = "explicit-revoked"
)

// selection is the resolved client-key choice plus the safe evidence a
// diagnostic can surface. A blocked/manual selection never carries a DSN.
type selection struct {
	Outcome   selectionOutcome
	Key       observedClientKey
	ActiveIDs []string // safe ids of the active candidates (for ambiguity)
}

// selectClientKey applies the deterministic selection rules. It never selects
// array[0]: a single active key is chosen only because it is the sole active
// candidate, and ambiguity blocks rather than picks. Repeated observation of the
// same keys yields the same outcome.
func selectClientKey(keys []observedClientKey, explicitID string) selection {
	active := make([]observedClientKey, 0, len(keys))
	for _, k := range keys {
		if k.Active {
			active = append(active, k)
		}
	}

	if explicitID != "" {
		for _, k := range keys {
			if k.ID == explicitID {
				if !k.Active {
					return selection{Outcome: selectionExplicitRevoked, Key: k}
				}
				return selection{Outcome: selectionSelected, Key: k}
			}
		}
		return selection{Outcome: selectionExplicitMissing}
	}

	switch len(active) {
	case 0:
		return selection{Outcome: selectionNoActive}
	case 1:
		return selection{Outcome: selectionSelected, Key: active[0]}
	default:
		ids := make([]string, 0, len(active))
		for _, k := range active {
			ids = append(ids, k.ID)
		}
		sort.Strings(ids)
		return selection{Outcome: selectionAmbiguous, ActiveIDs: ids}
	}
}
