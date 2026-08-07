package provider

import (
	"testing"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
)

// incompleteObservation is a client-key observation the host could not confirm
// exhaustive: it carries a next_cursor because more may remain unread.
func incompleteObservation(resources ...*providerv0.MaterialResourceObservation) *providerv0.MaterialObservation {
	return &providerv0.MaterialObservation{Complete: false, NextCursor: "more", Resources: resources}
}

func TestPlanBlocksSoleActiveKeyOnIncompleteObservation(t *testing.T) {
	// One active key looks sole-active, but the list is truncated: a second
	// active key could be on an unread page, so selection must block rather than
	// silently pick this one.
	obs := incompleteObservation(projectResource(true, "acme", "web"), clientKeyResource(keyA, true, dsnA))
	response := plan(t, planRequest(validInput(), obs, runtimeTarget()))

	if hasActionType(response, providerv0.ActionType_ACTION_TYPE_PROJECT_OUTPUT) {
		t.Fatal("an incomplete observation must not project from an unconfirmed sole-active key")
	}
	if !hasActionType(response, providerv0.ActionType_ACTION_TYPE_BLOCKED) {
		t.Fatal("an incomplete observation must block client-key selection")
	}
	if !hasDiagnostic(response.GetDiagnostics(), DiagOutcomeUnknown) {
		t.Fatal("an incomplete-observation block must be diagnosed")
	}
}

func TestPlanAllowsExplicitKeyOnIncompleteObservation(t *testing.T) {
	// An explicitly configured, present, active key was found by id, not inferred
	// from the list being exhaustive, so incompleteness does not block it.
	input := validInput()
	input["client_key_id"] = str(keyA)
	obs := incompleteObservation(projectResource(true, "acme", "web"), clientKeyResource(keyA, true, dsnA))
	response := plan(t, planRequest(input, obs, runtimeTarget()))

	if !hasActionType(response, providerv0.ActionType_ACTION_TYPE_PROJECT_OUTPUT) {
		t.Fatal("an explicit, present, active key must still project under an incomplete observation")
	}
}

func TestPlanBlocksNoActiveKeyOnIncompleteObservation(t *testing.T) {
	// Zero observed keys over an incomplete list is unknown, not empty: an active
	// key could be on an unread page. It must block as an unknown outcome, never
	// a MANUAL "no active client key; create one" verdict.
	obs := incompleteObservation(projectResource(true, "acme", "web"))
	response := plan(t, planRequest(validInput(), obs, runtimeTarget()))

	if hasActionType(response, providerv0.ActionType_ACTION_TYPE_MANUAL) {
		t.Fatal("an incomplete key list must not produce a definitive 'no active key' manual action")
	}
	if hasDiagnostic(response.GetDiagnostics(), DiagNoActiveKey) {
		t.Fatal("an incomplete key list must not be reported as definitively having no active key")
	}
	if !hasActionType(response, providerv0.ActionType_ACTION_TYPE_BLOCKED) || !hasDiagnostic(response.GetDiagnostics(), DiagOutcomeUnknown) {
		t.Fatal("an incomplete key list must block as an unknown outcome")
	}
}

func TestPlanBlocksExplicitMissingOnIncompleteObservation(t *testing.T) {
	// An explicit key not found in a truncated list could be on an unread page,
	// so it is unknown, not a definitive "not owned by this project".
	input := validInput()
	input["client_key_id"] = str(keyB)
	obs := incompleteObservation(projectResource(true, "acme", "web"), clientKeyResource(keyA, true, dsnA))
	response := plan(t, planRequest(input, obs, runtimeTarget()))

	if hasDiagnostic(response.GetDiagnostics(), DiagKeyMismatch) {
		t.Fatal("an explicit key missing from a truncated list must not be reported as definitively not owned")
	}
	if !hasDiagnostic(response.GetDiagnostics(), DiagOutcomeUnknown) {
		t.Fatal("an explicit key missing from an incomplete list must block as an unknown outcome")
	}
}

func TestPlanKeepsRevokedVerdictOnIncompleteObservation(t *testing.T) {
	// A positive find is definitive regardless of completeness: an explicit key
	// observed inactive is revoked whether or not the rest of the list was read.
	input := validInput()
	input["client_key_id"] = str(keyA)
	obs := incompleteObservation(projectResource(true, "acme", "web"), clientKeyResource(keyA, false, dsnA))
	response := plan(t, planRequest(input, obs, runtimeTarget()))

	if !hasDiagnostic(response.GetDiagnostics(), DiagKeyRevoked) {
		t.Fatal("an explicit, observed-inactive key must stay a definitive revoked verdict under incompleteness")
	}
	if hasDiagnostic(response.GetDiagnostics(), DiagOutcomeUnknown) {
		t.Fatal("a positive-find revocation must not be downgraded to an unknown outcome")
	}
}

func TestPlanReportsUnknownWhenProjectUnreadOnIncompleteObservation(t *testing.T) {
	// A project absent because its read failed transiently (incomplete
	// observation) is an unknown outcome, not a definitive inaccessibility.
	obs := incompleteObservation() // no project resource
	response := plan(t, planRequest(validInput(), obs, runtimeTarget()))

	if hasDiagnostic(response.GetDiagnostics(), DiagProjectInaccessible) {
		t.Fatal("a transiently-unread project must not be reported as definitively inaccessible")
	}
	if !hasDiagnostic(response.GetDiagnostics(), DiagOutcomeUnknown) {
		t.Fatal("a project unread over an incomplete observation must be an unknown outcome")
	}
}
