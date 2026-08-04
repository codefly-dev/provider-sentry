package provider

import (
	"sort"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
)

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
