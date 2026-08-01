package provider

import (
	"context"
	"fmt"
	"strings"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/canonical"
	"github.com/codefly-dev/core/provider/configuration"
	"github.com/codefly-dev/core/provider/sdk"
)

// Plan produces the exact ordered plan offline, without broker access. It never
// contacts Sentry; the host supplies the observation. The plan is observe/
// project only: it can express NO_OP, PROJECT_OUTPUT, BLOCKED, and
// MANUAL_ACTION, and structurally cannot express a remote mutation.
func (s *Server) Plan(_ context.Context, request *providerv0.PlanRequest) (*providerv0.PlanResponse, error) {
	ctx := request.GetContext()
	desired := request.GetDesired()
	binding := desired.GetBinding()
	if binding == nil {
		binding = ctx.GetBinding()
	}

	in, err := parseInputs(desired.GetInput())
	if err != nil {
		return &providerv0.PlanResponse{
			Diagnostics: []*basev0.FailureDiagnostic{diag(basev0.FailureDiagnostic_ERROR, DiagInvalidInput, err.Error())},
		}, nil
	}
	if diagnostics := validateInputs(in); hasError(diagnostics) {
		return &providerv0.PlanResponse{Diagnostics: diagnostics}, nil
	}

	accountID := ctx.GetAccountIdentity()

	// Project accessibility/identity gate applies before any projection: an
	// inaccessible or mismatched project blocks rather than projects.
	if observed := observedProjectFrom(request.GetObservation()); observed != nil {
		if blocked := s.gateProject(in, *observed, accountID); blocked != nil {
			plan, err := s.assemblePlan(request, binding, []*providerv0.PlanAction{blocked.action})
			if err != nil {
				return nil, err
			}
			return &providerv0.PlanResponse{Plan: plan, Diagnostics: []*basev0.FailureDiagnostic{blocked.diagnostic}}, nil
		}
	}

	var actions []*providerv0.PlanAction
	var diagnostics []*basev0.FailureDiagnostic
	if request.GetOutputTarget().GetContract() == configuration.ErrorTrackingBuildContract {
		actions, diagnostics = s.planBuild(in, request.GetOutputTarget(), desired.GetCredentialReferences(), accountID)
	} else {
		actions, diagnostics = s.planRuntime(in, request.GetOutputTarget(), observedClientKeys(request.GetObservation()), accountID)
	}

	plan, err := s.assemblePlan(request, binding, actions)
	if err != nil {
		return nil, err
	}
	return &providerv0.PlanResponse{Plan: plan, Diagnostics: diagnostics}, nil
}

type blockedProject struct {
	action     *providerv0.PlanAction
	diagnostic *basev0.FailureDiagnostic
}

// gateProject blocks when the observed project is inaccessible or is not the
// configured organization/project. Identity is matched on the configured slugs,
// never inferred.
func (s *Server) gateProject(in inputs, observed observedProject, accountID string) *blockedProject {
	mismatch := (observed.Slug != "" && observed.Slug != in.Project) ||
		(observed.OrgSlug != "" && observed.OrgSlug != in.Organization)
	if observed.Accessible && !mismatch {
		return nil
	}
	action, _ := sdk.NewBlockedAction("project-inaccessible", 0, resourceProject)
	action.Ownership = providerv0.Ownership_OWNERSHIP_OBSERVED
	action.RemoteIdentity = remoteIdentity(resourceProject, observed.RemoteID, accountID)
	message := fmt.Sprintf("the configured project %s/%s is not accessible with the setup credential", in.Organization, in.Project)
	if mismatch {
		message = fmt.Sprintf("the observed project (%s/%s) is not the configured project (%s/%s)", observed.OrgSlug, observed.Slug, in.Organization, in.Project)
	}
	action.Summary = message
	return &blockedProject{action: action, diagnostic: diag(basev0.FailureDiagnostic_ERROR, DiagProjectInaccessible, message)}
}

// planRuntime selects the client key and projects the runtime error-tracking
// contract. Zero active keys is a manual action; ambiguity or an explicit key
// that is missing/revoked blocks. Only a uniquely selected active key projects.
func (s *Server) planRuntime(in inputs, target *providerv0.OutputTarget, keys []observedClientKey, accountID string) ([]*providerv0.PlanAction, []*basev0.FailureDiagnostic) {
	sel := selectClientKey(keys, in.ClientKeyID)
	switch sel.Outcome {
	case selectionSelected:
		noop, _ := sdk.NewNoOpAction("client-key-selected", 0, resourceClientKey)
		noop.Ownership = providerv0.Ownership_OWNERSHIP_OBSERVED
		noop.RemoteIdentity = remoteIdentity(resourceClientKey, sel.Key.ID, accountID)
		noop.Summary = fmt.Sprintf("selected the sole active client key %s; observe-only, no remote mutation", sel.Key.ID)
		actions := []*providerv0.PlanAction{noop}

		var diagnostics []*basev0.FailureDiagnostic
		if in.ExpectedDSN != "" && in.ExpectedDSN != sel.Key.PublicDSN {
			diagnostics = append(diagnostics, diag(basev0.FailureDiagnostic_WARNING, DiagDSNMismatch,
				"the operator-supplied DSN does not match the selected client key's public DSN"))
		}
		if output := s.planErrorTracking(in, target, sel.Key, uint32(len(actions))); output != nil {
			actions = append(actions, output)
		}
		return actions, diagnostics

	case selectionNoActive:
		manual, _ := sdk.NewManualAction("client-key-none", 0, resourceClientKey)
		manual.Ownership = providerv0.Ownership_OWNERSHIP_OBSERVED
		manual.Summary = "no active client key exists for this project; create or activate one before error tracking can be configured"
		return []*providerv0.PlanAction{manual}, []*basev0.FailureDiagnostic{
			diag(basev0.FailureDiagnostic_ERROR, DiagNoActiveKey, "no active client key exists for the configured project"),
		}

	case selectionAmbiguous:
		blocked, _ := sdk.NewBlockedAction("client-key-ambiguous", 0, resourceClientKey)
		blocked.Ownership = providerv0.Ownership_OWNERSHIP_OBSERVED
		blocked.Summary = fmt.Sprintf("multiple active client keys (%s); select one explicitly with client_key_id", strings.Join(sel.ActiveIDs, ", "))
		return []*providerv0.PlanAction{blocked}, []*basev0.FailureDiagnostic{
			diag(basev0.FailureDiagnostic_ERROR, DiagAmbiguousKey,
				fmt.Sprintf("client-key selection is ambiguous across active keys %s; set client_key_id", strings.Join(sel.ActiveIDs, ", "))),
		}

	case selectionExplicitRevoked:
		blocked, _ := sdk.NewBlockedAction("client-key-revoked", 0, resourceClientKey)
		blocked.Ownership = providerv0.Ownership_OWNERSHIP_OBSERVED
		blocked.RemoteIdentity = remoteIdentity(resourceClientKey, sel.Key.ID, accountID)
		blocked.Summary = fmt.Sprintf("the configured client key %s is revoked; select an active key", sel.Key.ID)
		return []*providerv0.PlanAction{blocked}, []*basev0.FailureDiagnostic{
			diag(basev0.FailureDiagnostic_ERROR, DiagKeyRevoked, "the explicitly configured client key is revoked"),
		}

	default: // selectionExplicitMissing
		blocked, _ := sdk.NewBlockedAction("client-key-missing", 0, resourceClientKey)
		blocked.Ownership = providerv0.Ownership_OWNERSHIP_OBSERVED
		blocked.Summary = "the configured client_key_id is not a client key of this project"
		return []*providerv0.PlanAction{blocked}, []*basev0.FailureDiagnostic{
			diag(basev0.FailureDiagnostic_ERROR, DiagKeyMismatch, "the explicitly configured client key is not owned by the configured project"),
		}
	}
}

// planErrorTracking projects error-tracking@1 from the selected key's public
// DSN. The DSN is a public, browser-exposable value; no management token is ever
// projected.
func (s *Server) planErrorTracking(in inputs, target *providerv0.OutputTarget, key observedClientKey, position uint32) *providerv0.PlanAction {
	if target.GetContract() != configuration.ErrorTrackingContract || target.GetTargetGeneration() == 0 || key.PublicDSN == "" {
		return nil
	}
	values := map[string]*providerv0.OutputValue{
		"SENTRY_DSN": publicOutput(key.PublicDSN),
	}
	if in.Environment != "" {
		values["SENTRY_ENVIRONMENT"] = publicOutput(in.Environment)
	}
	if in.Release != "" {
		values["SENTRY_RELEASE"] = publicOutput(in.Release)
	}
	action := s.projectOutput("error-tracking-project", target, position, values)
	if action != nil {
		action.Summary = "project error-tracking@1 (public DSN for browser and backend; no management token)"
	}
	return action
}

// planBuild projects the build-only error-tracking-build@1 contract from the
// separate org:ci build credential. The build token is an opaque reference in a
// build consumer only; it never reaches runtime, and the setup token is never
// projected here or anywhere.
func (s *Server) planBuild(in inputs, target *providerv0.OutputTarget, references []*providerv0.OpaqueReference, accountID string) ([]*providerv0.PlanAction, []*basev0.FailureDiagnostic) {
	buildRef := referenceByPurpose(references, providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_BUILD)
	if buildRef == nil {
		noop, _ := sdk.NewNoOpAction("build-credential-absent", 0, resourceProject)
		noop.Ownership = providerv0.Ownership_OWNERSHIP_OBSERVED
		noop.Summary = "no org:ci build credential supplied; the optional error-tracking-build projection is omitted"
		return []*providerv0.PlanAction{noop}, []*basev0.FailureDiagnostic{
			diag(basev0.FailureDiagnostic_INFO, DiagScopeUnverified, "error-tracking-build is optional; supply an org:ci build credential to enable release and source-map uploads"),
		}
	}
	values := map[string]*providerv0.OutputValue{
		"SENTRY_AUTH_TOKEN": referenceOutput(buildRef),
		"SENTRY_ORG":        publicOutput(in.Organization),
		"SENTRY_PROJECT":    publicOutput(in.Project),
	}
	action := s.projectOutput("error-tracking-build-project", target, 0, values)
	if action == nil {
		return nil, nil
	}
	action.Summary = "project error-tracking-build@1 (org:ci token as an opaque build-only reference; never runtime)"
	// Sentry does not expose arbitrary token-scope introspection, so build-token
	// purpose fit is reported honestly as unverified rather than asserted.
	return []*providerv0.PlanAction{action}, []*basev0.FailureDiagnostic{
		diag(basev0.FailureDiagnostic_INFO, DiagScopeUnverified, "org:ci build-token scope fit is reported as unverified: Sentry does not expose token-scope introspection"),
	}
}

func (s *Server) projectOutput(id string, target *providerv0.OutputTarget, position uint32, values map[string]*providerv0.OutputValue) *providerv0.PlanAction {
	proposal := &providerv0.OutputProposal{
		Contract:         target.GetContract(),
		TargetGeneration: target.GetTargetGeneration(),
		Values:           values,
	}
	action, err := sdk.NewProjectOutputAction(id, position, proposal)
	if err != nil {
		return nil
	}
	return action
}

// assemblePlan binds the input digests and computes the plan digest.
func (s *Server) assemblePlan(request *providerv0.PlanRequest, binding *providerv0.BindingAddress, actions []*providerv0.PlanAction) (*providerv0.OrderedPlan, error) {
	plan := &providerv0.OrderedPlan{
		PlanId:         planID(binding, request.GetStateGeneration()),
		ArtifactDigest: s.artifactDigest,
		ManifestDigest: s.manifestDigest,
		CatalogDigest:  s.catalogDigest,
		Actions:        actions,
	}
	if desired := request.GetDesired(); desired != nil {
		digest, err := canonical.BindingDesiredStateDigest(desired)
		if err != nil {
			return nil, err
		}
		plan.DesiredDigest = digest
	}
	if observation := request.GetObservation(); observation != nil {
		digest, err := canonical.MaterialObservationDigest(observation)
		if err != nil {
			return nil, err
		}
		plan.ObservationDigest = digest
	}
	if target := request.GetOutputTarget(); target != nil {
		digest, err := canonical.OutputTargetDigest(target)
		if err != nil {
			return nil, err
		}
		plan.OutputTargetDigest = digest
	}
	if generation := request.GetStateGeneration(); generation != nil {
		digest, err := canonical.StateGenerationDigest(generation)
		if err != nil {
			return nil, err
		}
		plan.StateGenerationDigest = digest
	}
	if policy := request.GetPolicyInput(); policy != nil {
		digest, err := canonical.PolicyApprovalInputDigest(policy)
		if err != nil {
			return nil, err
		}
		plan.PolicyInputDigest = digest
	}
	return canonical.BindOrderedPlanDigest(plan)
}

func remoteIdentity(resourceType, remoteID, accountID string) *providerv0.RemoteIdentity {
	return &providerv0.RemoteIdentity{
		Provider:     "sentry",
		AccountId:    accountID,
		ResourceType: resourceType,
		RemoteId:     remoteID,
	}
}

func referenceByPurpose(references []*providerv0.OpaqueReference, purpose providerv0.CredentialPurpose) *providerv0.OpaqueReference {
	for _, reference := range references {
		if reference.GetPurpose() == purpose {
			return reference
		}
	}
	return nil
}

func publicOutput(value string) *providerv0.OutputValue {
	return &providerv0.OutputValue{Kind: &providerv0.OutputValue_PublicValue{PublicValue: publicStringValue(value)}}
}

func referenceOutput(reference *providerv0.OpaqueReference) *providerv0.OutputValue {
	return &providerv0.OutputValue{Kind: &providerv0.OutputValue_OpaqueReference{OpaqueReference: reference}}
}

func planID(binding *providerv0.BindingAddress, generation *providerv0.StateGeneration) string {
	return fmt.Sprintf("plan-%s-%s-%s-%d",
		binding.GetWorkspaceId(), binding.GetEnvironmentId(), binding.GetBindingId(), generation.GetGeneration())
}
