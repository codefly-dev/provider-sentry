package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/canonical"
	"github.com/codefly-dev/core/provider/manifest"
)

// Packaged request descriptor and origin rule identifiers. These must match the
// manifest; a mismatch fails closed when the request is built.
const (
	originRuleAPI          = "api"
	requestProjectRetrieve = "project.retrieve"
	requestClientKeyList   = "client-key.list"
)

// buildExecuteRequest assembles one host-admitted broker request from the
// admitted provider context and a packaged read descriptor. Every security-
// relevant field — the origin, the credential handle, the response policy — is
// taken from host-owned context, never fabricated by the provider. The request
// is GET-only: this provider packages no mutating descriptor.
func (s *Server) buildExecuteRequest(ctx *providerv0.ProviderContext, descriptorID string, pathParameters, query map[string]*providerv0.PublicValue) (*providerv0.ExecuteRequestRequest, error) {
	descriptor, ok := s.requestDescriptor(descriptorID)
	if !ok {
		return nil, fmt.Errorf("request descriptor %q is not packaged", descriptorID)
	}
	descriptorDigest, err := manifest.RequestDescriptorDigest(descriptor)
	if err != nil {
		return nil, err
	}
	origin, err := admittedAPIOrigin(ctx)
	if err != nil {
		return nil, err
	}
	handle, err := credentialHandle(ctx, providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_MANAGEMENT)
	if err != nil {
		return nil, err
	}
	policyDigest, err := s.responsePolicyDigest(descriptor)
	if err != nil {
		return nil, err
	}
	planned := &providerv0.PlannedRequest{
		RequestDescriptorId:     descriptor.ID,
		RequestDescriptorDigest: descriptorDigest,
		Method:                  providerv0.HTTPMethod_HTTP_METHOD_GET,
		AdmittedOriginDigest:    origin.GetAdmissionDigest(),
		PathParameters:          pathParameters,
		Query:                   query,
		CredentialPurposes:      []providerv0.CredentialPurpose{providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_MANAGEMENT},
		ResponsePolicyDigest:    policyDigest,
	}
	planned, err = canonical.BindPlannedRequestDigest(planned)
	if err != nil {
		return nil, err
	}
	return &providerv0.ExecuteRequestRequest{
		Context:           ctx,
		RequestId:         callbackID(ctx, descriptor.ID, query),
		Request:           planned,
		Origin:            origin,
		CredentialHandles: []*providerv0.CredentialHandle{handle},
	}, nil
}

// callbackID derives a per-callback request identity that is unique across the
// descriptors and pages issued within one attempt. Reusing a single id (e.g. the
// descriptor id) would collide across pagination pages and let a host dedup or
// reject distinct reads; scoping it to the attempt, descriptor, and cursor keeps
// each callback distinct while staying stable for a given logical read.
func callbackID(ctx *providerv0.ProviderContext, descriptorID string, query map[string]*providerv0.PublicValue) string {
	id := descriptorID
	if attempt := ctx.GetOperation().GetAttemptId(); attempt != "" {
		id = attempt + ":" + descriptorID
	}
	if cursor := query["cursor"].GetStringValue(); cursor != "" {
		id += ":" + cursor
	}
	return id
}

func (s *Server) requestDescriptor(id string) (manifest.RequestDescriptor, bool) {
	for _, descriptor := range s.manifest.Requests {
		if descriptor.ID == id {
			return descriptor, true
		}
	}
	return manifest.RequestDescriptor{}, false
}

// responsePolicyDigest binds the response schema a descriptor forwards through.
// It is stable across observations of the same manifest, so a cassette records
// and replays against a fixed policy identity.
func (s *Server) responsePolicyDigest(descriptor manifest.RequestDescriptor) (string, error) {
	for _, schema := range s.manifest.ResponseSchemas {
		if schema.ID != descriptor.ResponseSchema {
			continue
		}
		encoded, err := json.Marshal(schema)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(encoded)
		return "sha256:" + hex.EncodeToString(sum[:]), nil
	}
	return "", fmt.Errorf("response schema %q is not packaged", descriptor.ResponseSchema)
}

// admittedAPIOrigin returns the host-attested origin for the single bounded api
// rule. Absence means the host never admitted the request's origin, which is a
// hard stop rather than a silent fall-through to a default.
func admittedAPIOrigin(ctx *providerv0.ProviderContext) (*providerv0.AdmittedOrigin, error) {
	for _, origin := range ctx.GetAdmittedOrigins() {
		if origin.GetOriginRuleId() == originRuleAPI {
			return origin, nil
		}
	}
	return nil, fmt.Errorf("no host-admitted origin for rule %q", originRuleAPI)
}

// credentialHandle returns the admitted handle for a purpose. The setup token is
// a management-consumer credential; there is no other admitted purpose on a read.
func credentialHandle(ctx *providerv0.ProviderContext, purpose providerv0.CredentialPurpose) (*providerv0.CredentialHandle, error) {
	for _, handle := range ctx.GetCredentials() {
		if handle.GetPurpose() == purpose {
			return handle, nil
		}
	}
	return nil, fmt.Errorf("no host-admitted credential for purpose %s", purpose)
}

// projectFromResponse builds the project projection from the filtered forwarded
// fields of a successful project retrieve. A 200 response is itself the
// accessibility attestation; only safe identifiers reach this code.
func projectFromResponse(fields map[string]*providerv0.PublicValue) *observedProject {
	return &observedProject{
		RemoteID:   forwardedString(fields, "$.id"),
		Slug:       forwardedString(fields, "$.slug"),
		Name:       forwardedString(fields, "$.name"),
		OrgSlug:    forwardedString(fields, "$.organization.slug"),
		Status:     forwardedString(fields, "$.status"),
		Accessible: true,
	}
}

// clientKeysFromResponse regroups the flat, index-qualified forwarded selectors
// of a client-key list into per-key projections in cursor order. The response
// policy forwards only the public DSN and safe identifiers; no secret selector
// exists, so no secret can be regrouped here.
func clientKeysFromResponse(fields map[string]*providerv0.PublicValue) []observedClientKey {
	type raw struct {
		id        string
		active    bool
		publicDSN string
	}
	byIndex := map[int]*raw{}
	for selector, value := range fields {
		index, suffix, ok := indexedSelector(selector)
		if !ok {
			continue
		}
		entry := byIndex[index]
		if entry == nil {
			entry = &raw{}
			byIndex[index] = entry
		}
		switch suffix {
		case ".id":
			entry.id = value.GetStringValue()
		case ".isActive":
			entry.active = value.GetBoolValue()
		case ".dsn.public":
			entry.publicDSN = value.GetStringValue()
		}
	}
	indices := make([]int, 0, len(byIndex))
	for index := range byIndex {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	keys := make([]observedClientKey, 0, len(indices))
	for _, index := range indices {
		entry := byIndex[index]
		keys = append(keys, observedClientKey{ID: entry.id, Active: entry.active, PublicDSN: entry.publicDSN})
	}
	return keys
}

// indexedSelector splits a forwarded array selector such as "$[2].dsn.public"
// into its element index and the remaining field suffix (".dsn.public").
func indexedSelector(selector string) (int, string, bool) {
	if !strings.HasPrefix(selector, "$[") {
		return 0, "", false
	}
	close := strings.IndexByte(selector, ']')
	if close < 0 {
		return 0, "", false
	}
	index, err := strconv.Atoi(selector[2:close])
	if err != nil {
		return 0, "", false
	}
	return index, selector[close+1:], true
}

func forwardedString(fields map[string]*providerv0.PublicValue, selector string) string {
	return fields[selector].GetStringValue()
}
