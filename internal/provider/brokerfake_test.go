package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/canonical"
	"github.com/codefly-dev/core/provider/manifest"
	"github.com/codefly-dev/core/provider/responsepolicy"
	"google.golang.org/grpc"
)

// fakeHost is an in-process ProviderHost. It stands in for the running
// host+broker without a coordinator: it runs core's real responsepolicy filter
// over canned Sentry payloads, so response filtering is asserted on
// host-delivered bytes exactly as the broker would filter them (the pinned core
// broker cannot itself execute Sentry's two-segment paths, so the filter is
// exercised directly here rather than through broker.Session). It records the
// projections proposed to it so tests can assert exactly what would be
// committed.
type fakeHost struct {
	m *manifest.Manifest

	// responses maps a request descriptor id (optionally suffixed by "?cursor")
	// to a canned Sentry response.
	responses map[string]cannedResponse
	// strictSuppress marks SUPPRESS_REPORT_PRESENCE selectors required, so a
	// moved/renamed secret field fails closed rather than being silently dropped.
	strictSuppress bool

	proposals         []*providerv0.ProposeOutputRequest
	proposeDurable    bool
	proposeGeneration uint64

	// receivedRequestIDs records the callback id of every ExecuteRequest the
	// host was handed, so tests can assert each read carries a distinct identity.
	receivedRequestIDs []string
}

type cannedResponse struct {
	status uint32
	body   string
}

func newFakeHost(m *manifest.Manifest) *fakeHost {
	return &fakeHost{m: m, responses: map[string]cannedResponse{}, proposeDurable: true, proposeGeneration: 1}
}

func loadManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Load(loadManifestBytes(t))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	return m
}

func (h *fakeHost) set(descriptorID string, status uint32, body string) *fakeHost {
	h.responses[descriptorID] = cannedResponse{status: status, body: body}
	return h
}

func (h *fakeHost) setCursor(descriptorID, cursor string, status uint32, body string) *fakeHost {
	h.responses[descriptorID+"?"+cursor] = cannedResponse{status: status, body: body}
	return h
}

func (h *fakeHost) ExecuteRequest(ctx context.Context, in *providerv0.ExecuteRequestRequest, _ ...grpc.CallOption) (*providerv0.ExecuteRequestResponse, error) {
	h.receivedRequestIDs = append(h.receivedRequestIDs, in.GetRequestId())
	descriptorID := in.GetRequest().GetRequestDescriptorId()
	cursor := in.GetRequest().GetQuery()["cursor"].GetStringValue()
	canned, ok := h.responses[descriptorID+"?"+cursor]
	if !ok {
		canned, ok = h.responses[descriptorID]
	}
	if !ok {
		return nil, fmt.Errorf("no canned response for %q cursor %q", descriptorID, cursor)
	}
	if canned.status < 200 || canned.status >= 300 {
		// The broker drops a non-success body entirely and surfaces status alone.
		certainty := providerv0.OutcomeCertainty_OUTCOME_CERTAINTY_COMPLETE
		if canned.status >= 500 || (canned.status >= 300 && canned.status < 400) {
			certainty = providerv0.OutcomeCertainty_OUTCOME_CERTAINTY_UNCERTAIN
		}
		return &providerv0.ExecuteRequestResponse{
			RequestId:  in.GetRequestId(),
			StatusCode: canned.status,
			Delivery:   providerv0.DeliveryState_DELIVERY_STATE_RESPONSE_RECEIVED,
			Certainty:  certainty,
		}, nil
	}
	policy, err := h.policyFor(descriptorID)
	if err != nil {
		return nil, err
	}
	result, err := policy.Filter(ctx, []byte(canned.body), "", "application/json", noCaptureSink{})
	if err != nil {
		// Fail closed exactly as the broker does: a filtering failure surfaces as
		// a hard error with no forwarded bytes.
		return nil, err
	}
	response := &providerv0.ExecuteRequestResponse{
		RequestId:          in.GetRequestId(),
		StatusCode:         canned.status,
		Delivery:           providerv0.DeliveryState_DELIVERY_STATE_RESPONSE_RECEIVED,
		Certainty:          providerv0.OutcomeCertainty_OUTCOME_CERTAINTY_COMPLETE,
		SuppressedPresence: result.Suppressed,
	}
	for _, forwarded := range result.Forwarded {
		response.Forwarded = append(response.Forwarded, &providerv0.FilteredField{Selector: forwarded.Selector, Value: forwarded.Value})
	}
	return response, nil
}

func (h *fakeHost) ProposeOutput(_ context.Context, in *providerv0.ProposeOutputRequest, _ ...grpc.CallOption) (*providerv0.ProposeOutputResponse, error) {
	h.proposals = append(h.proposals, in)
	if !h.proposeDurable {
		return &providerv0.ProposeOutputResponse{Durable: false}, nil
	}
	return &providerv0.ProposeOutputResponse{
		Durable:    true,
		Generation: h.proposeGeneration,
		Digest:     in.GetProposal().GetDigest(),
	}, nil
}

// policyFor compiles the manifest response schema a descriptor forwards through
// into a host response policy, mirroring the broker's derivation. strictSuppress
// makes the legacy secret suppressions required so a renamed secret fails closed.
func (h *fakeHost) policyFor(descriptorID string) (responsepolicy.Policy, error) {
	var schemaID string
	for _, descriptor := range h.m.Requests {
		if descriptor.ID == descriptorID {
			schemaID = descriptor.ResponseSchema
		}
	}
	var fields []responsepolicy.Field
	for _, schema := range h.m.ResponseSchemas {
		if schema.ID != schemaID {
			continue
		}
		for _, field := range schema.Fields {
			policyField := responsepolicy.Field{Selector: field.Selector, Disposition: field.Disposition}
			if h.strictSuppress && field.Disposition == manifest.ResponseSuppressPresence {
				policyField.Required = true
			}
			fields = append(fields, policyField)
		}
	}
	if len(fields) == 0 {
		return responsepolicy.Policy{}, fmt.Errorf("response schema %q not found", schemaID)
	}
	return responsepolicy.Policy{Fields: fields, Limits: responsepolicy.DefaultLimits()}, nil
}

// noCaptureSink fails if ever asked to store a secret. The Sentry manifest
// declares no capture selector, so it must never be called.
type noCaptureSink struct{}

func (noCaptureSink) Put(context.Context, responsepolicy.SinkTarget, string) (*providerv0.OpaqueReference, error) {
	return nil, fmt.Errorf("no Sentry response field is ever captured")
}

// hostContext builds an admitted provider context: one bounded api origin and
// one setup credential handle, matching what the running host would supply for a
// read.
func hostContext(input map[string]*providerv0.PublicValue) *providerv0.ProviderContext {
	origin := &providerv0.AdmittedOrigin{
		OriginRuleId:        originRuleAPI,
		Scheme:              "https",
		Host:                "sentry.io",
		Port:                443,
		PrivateNetworkClass: providerv0.PrivateNetworkClass_PRIVATE_NETWORK_CLASS_PUBLIC,
	}
	if digest, err := canonical.AdmittedOriginDigest(origin); err == nil {
		origin.AdmissionDigest = digest
	}
	return &providerv0.ProviderContext{
		Offline: &providerv0.OfflineProviderContext{
			Binding:         binding(),
			Mode:            providerv0.HostMode_HOST_MODE_DEVELOPMENT,
			AccountIdentity: "acme",
			Input:           input,
		},
		Credentials:     []*providerv0.CredentialHandle{{Handle: "setup-handle", Purpose: providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_MANAGEMENT}},
		AdmittedOrigins: []*providerv0.AdmittedOrigin{origin},
		Operation:       &providerv0.OperationIdentity{OperationId: "op1", AttemptId: "att1", ActionId: "a1", PlanId: "plan1"},
		Budget:          &providerv0.RequestBudget{RequestCount: 8, RequestBytes: 8192, ResponseBytes: 262144},
	}
}

// hostServer builds a server wired to the given fake host.
func hostServer(t *testing.T, host Host) *Server {
	t.Helper()
	data := loadManifestBytes(t)
	m, err := manifest.Load(data)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	digest, err := m.Digest()
	if err != nil {
		t.Fatalf("manifest digest: %v", err)
	}
	server, err := NewServer(data, Identity{
		Publisher:      m.Agent.Publisher,
		Name:           m.Agent.Name,
		Version:        m.Agent.Version,
		ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
		ManifestDigest: digest,
	}, WithHost(host))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return server
}
