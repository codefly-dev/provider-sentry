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
