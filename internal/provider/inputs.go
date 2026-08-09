package provider

import (
	"fmt"
	"strings"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/configuration"
)

// inputs is the typed, normalized, non-secret desired input for an
// error-tracking binding. No secret ever appears here: the setup and build
// credentials arrive as opaque credential references on the desired state, and
// the DSN is a public value.
type inputs struct {
	Origin       origin
	Organization string
	Project      string
	ClientKeyID  string // optional explicit client-key selection
	ExpectedDSN  string // optional operator-supplied public DSN to reconcile against
	Environment  string
	Release      string
}

// parseInputs reads the typed inputs from a normalized public-value map. It only
// reads declared keys and rejects secret-shaped literals, which must never
// reach the provider as raw input.
func parseInputs(raw map[string]*providerv0.PublicValue) (inputs, error) {
	var in inputs
	for key, value := range raw {
		if s, ok := publicString(value); ok && configuration.LooksSecret(s) {
			return inputs{}, fmt.Errorf("input %q carries a secret-shaped literal; setup and build credentials must be opaque references", key)
		}
	}
	in.Origin = parseOrigin(strings.TrimSpace(stringInput(raw, "api_origin")))
	in.Organization = strings.TrimSpace(stringInput(raw, "organization_slug"))
	in.Project = strings.TrimSpace(stringInput(raw, "project_slug"))
	in.ClientKeyID = strings.TrimSpace(stringInput(raw, "client_key_id"))
	in.ExpectedDSN = strings.TrimSpace(stringInput(raw, "expected_dsn"))
	in.Environment = strings.TrimSpace(stringInput(raw, "environment"))
	in.Release = strings.TrimSpace(stringInput(raw, "release"))
	return in, nil
}

func stringInput(raw map[string]*providerv0.PublicValue, key string) string {
	s, _ := publicString(raw[key])
	return s
}

// publicString returns the string payload of a public value when it is a string.
func publicString(v *providerv0.PublicValue) (string, bool) {
	if v == nil {
		return "", false
	}
	if _, ok := v.GetKind().(*providerv0.PublicValue_StringValue); ok {
		return v.GetStringValue(), true
	}
	return "", false
}

func publicStringValue(s string) *providerv0.PublicValue {
	return &providerv0.PublicValue{Kind: &providerv0.PublicValue_StringValue{StringValue: s}}
}

func publicBoolValue(b bool) *providerv0.PublicValue {
	return &providerv0.PublicValue{Kind: &providerv0.PublicValue_BoolValue{BoolValue: b}}
}
