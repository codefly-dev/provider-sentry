package provider

import (
	"context"
	"sort"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/sdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Observe reads the configured Sentry project and its client keys through the
// host broker and projects only the Codefly-owned, non-secret fields Plan
// consumes. It never opens a socket and never sees a raw credential: every read
// is a host-admitted ExecuteRequest whose response is already filtered so only
// the public DSN and safe identifiers arrive. A read it cannot complete
// (rate-limited, uncertain delivery) yields an explicitly incomplete
// observation rather than a partial list a later selection could misread.
func (s *Server) Observe(ctx context.Context, request *providerv0.ObserveRequest) (*providerv0.ObserveResponse, error) {
	if s.host == nil {
		return nil, status.Error(codes.Unavailable, "provider host callback is not configured")
	}
	pctx := request.GetContext()
	in, err := parseInputs(pctx.GetOffline().GetInput())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if diagnostics := validateInputs(in); hasError(diagnostics) {
		return nil, status.Error(codes.InvalidArgument, "desired input is not valid for observation")
	}
	accountID := pctx.GetOffline().GetAccountIdentity()

	var resources []*providerv0.MaterialResourceObservation
	var diagnostics []*basev0.FailureDiagnostic
	complete := true
	retryAfter := false

	projectParams := map[string]*providerv0.PublicValue{
		"organization_slug": publicStringValue(in.Organization),
		"project_slug":      publicStringValue(in.Project),
	}
	projectRead, err := s.read(ctx, pctx, requestProjectRetrieve, projectParams, nil)
	if err != nil {
		return nil, err
	}
	if projectRead.diagnostic != nil {
		diagnostics = append(diagnostics, projectRead.diagnostic)
	}
	retryAfter = retryAfter || projectRead.retryAfter
	complete = complete && !projectRead.uncertain
	if projectRead.ok {
		resources = append(resources, projectObservation(projectFromResponse(projectRead.fields), accountID))
	}

	keyParams := map[string]*providerv0.PublicValue{
		"organization_slug": publicStringValue(in.Organization),
		"project_slug":      publicStringValue(in.Project),
	}
	var keyQuery map[string]*providerv0.PublicValue
	if cursor := request.GetCursor(); cursor != "" {
		keyQuery = map[string]*providerv0.PublicValue{"cursor": publicStringValue(cursor)}
	}
	keyRead, err := s.read(ctx, pctx, requestClientKeyList, keyParams, keyQuery)
	if err != nil {
		return nil, err
	}
	if keyRead.diagnostic != nil {
		diagnostics = append(diagnostics, keyRead.diagnostic)
	}
	retryAfter = retryAfter || keyRead.retryAfter
	complete = complete && !keyRead.uncertain
	if keyRead.ok {
		for _, key := range clientKeysFromResponse(keyRead.fields) {
			resources = append(resources, clientKeyObservation(key, accountID))
		}
	}

	material := &providerv0.MaterialObservation{
		AccountIdentity: accountID,
		Mode:            pctx.GetOffline().GetMode(),
		Complete:        complete,
		Resources:       resources,
	}
	// An incomplete observation must name where the host resumes. A read that
	// could not be confirmed is resumed from the request cursor (or restarted),
	// never silently accepted as a full list.
	if !complete {
		material.NextCursor = request.GetCursor()
		if material.NextCursor == "" {
			material.NextCursor = observeRestartCursor
		}
	}
	volatile := &providerv0.VolatileObservation{Diagnostics: diagnostics}
	if retryAfter {
		volatile.RetryAfter = durationpb.New(rateLimitBackoff)
	}
	return sdk.Observation(material, volatile)
}

// rateLimitBackoff is the conservative fixed retry hint surfaced when Sentry
// reports a rate limit. The exact Retry-After window is a host/coordinator
// concern; the provider only signals that observation must back off.
const rateLimitBackoff = 30 * time.Second

// observeRestartCursor marks an incomplete observation with no prior cursor: the
// host resumes by restarting the observation once the transient condition
// clears. It is a resume marker, never a Sentry-issued cursor.
const observeRestartCursor = "restart"

// readOutcome is the classified result of one admitted read callback. Exactly
// one of ok / uncertain / a definitive non-success is set; fields are present
// only on a 2xx.
type readOutcome struct {
	fields     map[string]*providerv0.PublicValue
	statusCode uint32
	ok         bool
	uncertain  bool
	retryAfter bool
	diagnostic *basev0.FailureDiagnostic
}

// read executes one host-admitted read and classifies its outcome. A 2xx yields
// the filtered fields; a definitive 4xx yields a diagnostic; a 429, 5xx, or
// uncertain delivery is flagged uncertain so the observation is marked
// incomplete rather than trusted as a full list.
func (s *Server) read(ctx context.Context, pctx *providerv0.ProviderContext, descriptorID string, pathParameters, query map[string]*providerv0.PublicValue) (readOutcome, error) {
	execute, err := s.buildExecuteRequest(pctx, descriptorID, pathParameters, query)
	if err != nil {
		return readOutcome{}, status.Error(codes.FailedPrecondition, err.Error())
	}
	response, err := s.host.ExecuteRequest(ctx, execute)
	if err != nil {
		// A broker-level refusal (budget, admission, an over-budget/truncated
		// body) leaves the remote state unknown: fail safe as uncertain.
		return readOutcome{
			uncertain:  true,
			diagnostic: diag(basev0.FailureDiagnostic_WARNING, DiagOutcomeUnknown, "the host could not complete the read: "+err.Error()),
		}, nil
	}
	code := response.GetStatusCode()
	if code >= 200 && code < 300 {
		fields, err := sdk.DecodeFilteredResponse(response)
		if err != nil {
			return readOutcome{}, status.Error(codes.Internal, err.Error())
		}
		return readOutcome{fields: fields, statusCode: code, ok: true}, nil
	}
	diagCode := ClassifySentryError(SentryError{StatusCode: int(code), RetryAfter: code == 429})
	uncertain := code == 429 || code >= 500 || (code >= 300 && code < 400)
	return readOutcome{
		statusCode: code,
		uncertain:  uncertain,
		retryAfter: code == 429,
		diagnostic: diag(basev0.FailureDiagnostic_WARNING, diagCode, diagnosticMessageForStatus(code)),
	}, nil
}

func diagnosticMessageForStatus(code uint32) string {
	switch {
	case code == 401:
		return "the setup credential was not accepted for this project"
	case code == 403:
		return "the setup credential lacks project read scope"
	case code == 404:
		return "the configured project was not found"
	case code == 429:
		return "Sentry rate-limited the read; back off and retry"
	case code >= 500:
		return "Sentry returned a server error; the read outcome is unknown"
	default:
		return "the read did not succeed"
	}
}

// projectObservation projects the safe project fields Plan's gate consumes. A
// successful retrieve is the accessibility attestation, so accessible is true.
func projectObservation(project *observedProject, accountID string) *providerv0.MaterialResourceObservation {
	return &providerv0.MaterialResourceObservation{
		Identity:  remoteIdentity(resourceProject, project.RemoteID, accountID),
		Ownership: providerv0.Ownership_OWNERSHIP_OBSERVED,
		ProviderOwnedFields: map[string]*providerv0.PublicValue{
			fieldSlug:       publicStringValue(project.Slug),
			fieldName:       publicStringValue(project.Name),
			fieldOrgSlug:    publicStringValue(project.OrgSlug),
			fieldStatus:     publicStringValue(project.Status),
			fieldAccessible: publicBoolValue(true),
		},
	}
}

// clientKeyObservation projects one client key. Only the safe id/active flag and
// the public DSN are present; no secret is, because the response policy never
// forwarded one.
func clientKeyObservation(key observedClientKey, accountID string) *providerv0.MaterialResourceObservation {
	return &providerv0.MaterialResourceObservation{
		Identity:  remoteIdentity(resourceClientKey, key.ID, accountID),
		Ownership: providerv0.Ownership_OWNERSHIP_OBSERVED,
		ProviderOwnedFields: map[string]*providerv0.PublicValue{
			fieldActive:    publicBoolValue(key.Active),
			fieldPublicDSN: publicStringValue(key.PublicDSN),
		},
	}
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
	Explicit  bool     // the key was chosen by an explicit client_key_id, not by sole-active
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
					return selection{Outcome: selectionExplicitRevoked, Key: k, Explicit: true}
				}
				return selection{Outcome: selectionSelected, Key: k, Explicit: true}
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

// exhaustivenessDependent reports whether a selection outcome relies on having
// observed the complete client-key list. Sole-active (the key is the only active
// one), no-active (none is active anywhere), and explicit-missing (the key
// exists nowhere) are all valid only over a complete list; an unread page could
// overturn each. A key confirmed by a positive find — explicit active (selected)
// or explicit inactive (revoked) — and an already-ambiguous result do not depend
// on the list being exhaustive.
func exhaustivenessDependent(sel selection) bool {
	switch sel.Outcome {
	case selectionNoActive, selectionExplicitMissing:
		return true
	case selectionSelected:
		return !sel.Explicit
	default: // selectionAmbiguous, selectionExplicitRevoked
		return false
	}
}
