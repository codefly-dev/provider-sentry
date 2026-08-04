package provider

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/network/urlguard"
	"github.com/codefly-dev/core/provider/broker"
	"github.com/codefly-dev/core/provider/canonical"
	"github.com/codefly-dev/core/provider/cassette"
	"github.com/codefly-dev/core/provider/credentials"
	"github.com/codefly-dev/core/provider/manifest"
	"github.com/codefly-dev/core/provider/responsepolicy"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// setupToken is the poison Sentry setup credential the fake host injects as a
// bearer token. It must never appear in any recorded response, receipt, or
// state. legacySecret / dsnSecret are the client-key secrets the manifest
// suppresses; they too must never survive filtering.
const (
	setupToken     = "sntrys_" + "0123456789abcdefghijklmnopqrstuvwxyz"
	legacySecret   = "srt_" + "legacy_client_key_secret_value"
	dsnSecret      = "https://pub:srt_dsn_secret_value@sentry.io/1"
	sentryPublicIP = "203.0.113.10" // TEST-NET-3: classified public, never routed.
)

// fakeHost is an in-process ProviderHost backed by the real broker, credential
// vault, capture sink, and a record/replay cassette. It stands in for the
// coordinator so the broker-driven provider surface can be exercised end to end
// without a running host: replay performs no network I/O, and record reaches
// only a local Sentry stand-in through a pinned public origin.
type fakeHost struct {
	t           *testing.T
	manifest    *manifest.Manifest
	vault       *credentials.Vault
	sink        *captureSink
	checkpoints *checkpointStore
	cassette    *cassette.Cassette
	clientFor   func(urlguard.Origin, urlguard.Resolution) *http.Client
	resolver    *net.Resolver
	outputs     []*providerv0.OutputProposal
	now         func() time.Time
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	m, err := manifest.Load(loadManifestBytes(t))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	fixedNow := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return &fakeHost{
		t:           t,
		manifest:    m,
		vault:       credentials.NewVault().WithClock(func() time.Time { return fixedNow }),
		sink:        &captureSink{t: t},
		checkpoints: &checkpointStore{latest: map[string]*providerv0.ActionCheckpoint{}},
		now:         func() time.Time { return fixedNow },
	}
}

// record points the host at a local Sentry stand-in and a record cassette.
func (h *fakeHost) record(server *httptest.Server) {
	h.cassette = cassette.New(cassette.ModeRecord, h.manifest.Agent.Version)
	h.clientFor = dialLocal(h.t, server.Listener.Addr().String())
	h.resolver = stubResolver(h.t, sentryPublicIP)
}

// replay reloads the recorded cassette so subsequent calls serve filtered
// responses with no network reachable, and fails if any secret was persisted.
func (h *fakeHost) replay() {
	data, err := h.cassette.Marshal()
	if err != nil {
		h.t.Fatalf("marshal cassette: %v", err)
	}
	for _, secret := range []string{setupToken, legacySecret, dsnSecret, "srt_dsn_secret_value"} {
		if strings.Contains(string(data), secret) {
			h.t.Fatalf("cassette persisted a secret: %q", secret)
		}
	}
	loaded, err := cassette.Load(data, h.manifest.Agent.Version)
	if err != nil {
		h.t.Fatalf("load cassette: %v", err)
	}
	h.cassette = loaded
	h.clientFor = nil
	h.resolver = nil
}

func (h *fakeHost) ExecuteRequest(ctx context.Context, request *providerv0.ExecuteRequestRequest, _ ...grpc.CallOption) (*providerv0.ExecuteRequestResponse, error) {
	action, err := h.brokerAction(request)
	if err != nil {
		return nil, err
	}
	bound, err := h.bindCredentials(request)
	if err != nil {
		return nil, err
	}
	session, err := broker.New(broker.Config{
		Manifest:    h.manifest,
		Action:      action,
		Binding:     request.GetContext().GetOffline().GetBinding(),
		Budget:      request.GetContext().GetBudget(),
		ReadOnly:    true,
		Vault:       h.vault,
		Sink:        h.sink,
		Checkpoints: h.checkpoints,
		Cassette:    h.cassette,
		ClientFor:   h.clientFor,
		Resolver:    h.resolver,
		Deadlines:   urlguard.DefaultDeadlines(),
		Now:         h.now,
	})
	if err != nil {
		return nil, err
	}
	return session.Execute(ctx, bound)
}

func (h *fakeHost) RecordCheckpoint(_ context.Context, request *providerv0.RecordCheckpointRequest, _ ...grpc.CallOption) (*providerv0.RecordCheckpointResponse, error) {
	h.checkpoints.put(request.GetCheckpoint())
	return &providerv0.RecordCheckpointResponse{Durable: true, StateGeneration: 1}, nil
}

func (h *fakeHost) ResolveCapture(_ context.Context, _ *providerv0.ResolveCaptureRequest, _ ...grpc.CallOption) (*providerv0.ResolveCaptureResponse, error) {
	// This provider captures nothing; a resolve request would be a defect.
	h.t.Fatal("ResolveCapture must never be called: this provider captures no secrets")
	return nil, nil
}

func (h *fakeHost) ProposeOutput(_ context.Context, request *providerv0.ProposeOutputRequest, _ ...grpc.CallOption) (*providerv0.ProposeOutputResponse, error) {
	h.outputs = append(h.outputs, request.GetProposal())
	return &providerv0.ProposeOutputResponse{Durable: true, Generation: 1, Digest: request.GetProposal().GetDigest()}, nil
}

// brokerAction synthesizes the host-owned action that binds this request. The
// coordinator would carry it from the admitted plan; the fake host reconstructs
// exactly the identity binding the read-only descriptor requires — the single
// organization-slug remote id — so the broker's path, origin, and read-only
// admission still run for real.
func (h *fakeHost) brokerAction(request *providerv0.ExecuteRequestRequest) (*providerv0.PlanAction, error) {
	planned := request.GetRequest()
	var descriptor manifest.RequestDescriptor
	for _, d := range h.manifest.Requests {
		if d.ID == planned.GetRequestDescriptorId() {
			descriptor = d
		}
	}
	name := descriptor.RemoteIDParameters[0]
	action := &providerv0.PlanAction{
		ActionId:     request.GetContext().GetOperation().GetActionId(),
		Type:         providerv0.ActionType_ACTION_TYPE_NO_OP,
		ResourceType: descriptor.ResourceType,
		Ownership:    providerv0.Ownership_OWNERSHIP_OBSERVED,
		RemoteIdentity: &providerv0.RemoteIdentity{
			Provider: "sentry", ResourceType: descriptor.ResourceType,
			RemoteId: planned.GetPathParameters()[name].GetStringValue(),
		},
		Requests: []*providerv0.PlannedRequest{planned},
	}
	if err := canonical.ValidatePlanAction(action); err != nil {
		return nil, err
	}
	return action, nil
}

// bindCredentials mints a vault handle bound to the exact request digest and
// substitutes it for the provider's opaque purpose placeholder — the credential
// binding a real host owns.
func (h *fakeHost) bindCredentials(request *providerv0.ExecuteRequestRequest) (*providerv0.ExecuteRequestRequest, error) {
	origin := urlguard.Origin{Scheme: request.GetOrigin().GetScheme(), Host: request.GetOrigin().GetHost(), Port: request.GetOrigin().GetPort()}
	handles := make([]*providerv0.CredentialHandle, 0, len(request.GetRequest().GetCredentialPurposes()))
	for _, purpose := range request.GetRequest().GetCredentialPurposes() {
		handle, err := h.vault.Mint(setupToken, credentials.Scope{
			Principal:      "user",
			Organization:   "org",
			ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
			Binding:        request.GetContext().GetOffline().GetBinding(),
			PlanID:         request.GetContext().GetOperation().GetPlanId(),
			ActionID:       request.GetContext().GetOperation().GetActionId(),
			RequestDigest:  request.GetRequest().GetRequestDigest(),
			Purpose:        purpose,
			Origin:         origin,
			Method:         request.GetRequest().GetMethod(),
			Injection:      credentials.Injection{Kind: credentials.InjectBearer},
			MaxUses:        1,
			TTL:            time.Hour,
		})
		if err != nil {
			return nil, err
		}
		handles = append(handles, handle)
	}
	bound := proto.Clone(request).(*providerv0.ExecuteRequestRequest)
	bound.Context.Credentials = handles
	bound.CredentialHandles = handles
	return bound, nil
}

// fakeServer builds a Server whose identity matches the packaged manifest and
// whose host callback channel is the given fake host.
func fakeServer(t *testing.T, host *fakeHost) *Server {
	t.Helper()
	data := loadManifestBytes(t)
	m, err := manifest.Load(data)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	manifestDigest, err := m.Digest()
	if err != nil {
		t.Fatalf("manifest digest: %v", err)
	}
	server, err := NewServer(data, Identity{
		Publisher:      m.Agent.Publisher,
		Name:           m.Agent.Name,
		Version:        m.Agent.Version,
		ArtifactDigest: "sha256:" + strings.Repeat("b", 64),
		ManifestDigest: manifestDigest,
	}, WithHost(host))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return server
}

func admittedAPIOrigin(t *testing.T) *providerv0.AdmittedOrigin {
	t.Helper()
	origin := &providerv0.AdmittedOrigin{
		OriginRuleId:        originRuleAPI,
		Scheme:              "https",
		Host:                "sentry.io",
		Port:                443,
		PrivateNetworkClass: providerv0.PrivateNetworkClass_PRIVATE_NETWORK_CLASS_PUBLIC,
	}
	digest, err := canonical.AdmittedOriginDigest(origin)
	if err != nil {
		t.Fatalf("origin digest: %v", err)
	}
	origin.AdmissionDigest = digest
	return origin
}

// brokerContext builds a provider context with the admitted origin, a setup
// credential handle placeholder, and a host budget.
func brokerContext(t *testing.T, operationID, actionID string, input map[string]*providerv0.PublicValue) *providerv0.ProviderContext {
	t.Helper()
	return &providerv0.ProviderContext{
		Offline: &providerv0.OfflineProviderContext{
			Binding:         binding(),
			Mode:            providerv0.HostMode_HOST_MODE_DEVELOPMENT,
			Input:           input,
			AccountIdentity: "acme",
		},
		Credentials:     []*providerv0.CredentialHandle{{Handle: "placeholder-setup", Purpose: providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_MANAGEMENT}},
		AdmittedOrigins: []*providerv0.AdmittedOrigin{admittedAPIOrigin(t)},
		Operation:       &providerv0.OperationIdentity{OperationId: operationID, AttemptId: "attempt-1", ActionId: actionID, PlanId: "plan-1"},
		Budget:          &providerv0.RequestBudget{RequestCount: 20, RequestBytes: 8192, ResponseBytes: 262144},
	}
}

// sentryStub is a local Sentry API stand-in served over the record path.
func sentryStub(t *testing.T, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for pattern, handler := range routes {
		mux.HandleFunc(pattern, handler)
	}
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	return server
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// projectJSON renders one Sentry project object.
func projectJSON(id, slug, name, status string) string {
	encoded, _ := json.Marshal(map[string]any{
		"id": id, "slug": slug, "name": name, "status": status, "platform": "node",
	})
	return string(encoded)
}

func projectListJSON(projects ...string) string {
	return "[" + strings.Join(projects, ",") + "]"
}

// clientKeyJSON renders one Sentry client key object, always carrying the legacy
// `secret` and the credentialed `dsn.secret` the manifest must suppress.
func clientKeyJSON(id, projectID string, isActive bool, publicDSN string) string {
	object := map[string]any{
		"id":        id,
		"name":      "Default",
		"projectId": projectID,
		"isActive":  isActive,
		"public":    "pub_" + id,
		"secret":    legacySecret,
		"rateLimit": map[string]any{"window": 60, "count": 100},
		"dsn": map[string]any{
			"public": publicDSN,
			"secret": dsnSecret,
		},
	}
	encoded, _ := json.Marshal(object)
	return string(encoded)
}

func clientKeyListJSON(keys ...string) string {
	return "[" + strings.Join(keys, ",") + "]"
}

// checkpointStore is the durable checkpoint read/write side the broker depends
// on, keyed by action so the latest pre-send checkpoint is returned.
type checkpointStore struct {
	latest map[string]*providerv0.ActionCheckpoint
}

func (c *checkpointStore) put(checkpoint *providerv0.ActionCheckpoint) {
	c.latest[checkpoint.GetOperation().GetActionId()] = checkpoint
}

func (c *checkpointStore) Latest(_ context.Context, operation *providerv0.OperationIdentity) (*providerv0.ActionCheckpoint, error) {
	return c.latest[operation.GetActionId()], nil
}

// captureSink fails the test if the broker ever tries to capture a secret: the
// Sentry manifest suppresses every secret, so no capture must ever occur.
type captureSink struct{ t *testing.T }

func (s *captureSink) Put(context.Context, responsepolicy.SinkTarget, string) (*providerv0.OpaqueReference, error) {
	s.t.Fatal("no client-key secret must ever be captured; the manifest suppresses them")
	return nil, nil
}

// dialLocal returns a ClientFor that reaches a local server regardless of the
// pinned public IP, so the record path exercises real delivery and filtering
// against a Sentry stand-in.
func dialLocal(t *testing.T, addr string) func(urlguard.Origin, urlguard.Resolution) *http.Client {
	t.Helper()
	return func(urlguard.Origin, urlguard.Resolution) *http.Client {
		return &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, addr)
				},
				// The stand-in serves a self-signed cert for 127.0.0.1 while the
				// request targets sentry.io; verification is irrelevant to what these
				// tests exercise (filtering, suppression, admission).
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only stand-in
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
}

// stubResolver answers every A query with one fixed public IP so the broker's
// SSRF resolution admits sentry.io as a public origin without real DNS.
func stubResolver(t *testing.T, ip string) *net.Resolver {
	t.Helper()
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen dns: %v", err)
	}
	t.Cleanup(func() { _ = packet.Close() })
	answer := net.ParseIP(ip).To4()
	go func() {
		buffer := make([]byte, 512)
		for {
			n, from, err := packet.ReadFrom(buffer)
			if err != nil {
				return
			}
			response := dnsAnswer(buffer[:n], answer)
			if response != nil {
				_, _ = packet.WriteTo(response, from)
			}
		}
	}()
	stubAddr := packet.LocalAddr().String()
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "udp", stubAddr)
		},
	}
}

// dnsAnswer builds an A-record reply for query, or a bare no-answer reply for a
// non-A question, echoing the question section verbatim.
func dnsAnswer(query []byte, ip net.IP) []byte {
	if len(query) < 12 {
		return nil
	}
	end := 12
	for end < len(query) && query[end] != 0 {
		end += int(query[end]) + 1
	}
	if end >= len(query) || end+5 > len(query) {
		return nil
	}
	questionEnd := end + 5 // zero label + qtype(2) + qclass(2)
	qtype := int(query[end+1])<<8 | int(query[end+2])
	response := make([]byte, 0, questionEnd+16)
	response = append(response, query[0], query[1], 0x81, 0x80, 0x00, 0x01) // id, flags, qdcount
	if qtype == 1 {
		response = append(response, 0x00, 0x01) // ancount
	} else {
		response = append(response, 0x00, 0x00)
	}
	response = append(response, 0x00, 0x00, 0x00, 0x00) // nscount, arcount
	response = append(response, query[12:questionEnd]...)
	if qtype == 1 {
		response = append(response,
			0xc0, 0x0c, // name pointer to question
			0x00, 0x01, 0x00, 0x01, // type A, class IN
			0x00, 0x00, 0x00, 0x1e, // ttl 30s
			0x00, 0x04, // rdlength
			ip[0], ip[1], ip[2], ip[3])
	}
	return response
}
