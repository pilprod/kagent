package mcprelaytransport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/core/v2/mcprelay"
	"github.com/kagent-dev/kagent/go/core/v2/translator"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

var relayTestNow = time.Date(2026, time.August, 26, 18, 0, 0, 0, time.UTC)

type testPolicyStore struct {
	policy translator.MCPPolicyV1
}

func (s *testPolicyStore) MCPPolicy(context.Context, string) (translator.MCPPolicyV1, error) {
	return s.policy, nil
}

type testGrantVerifier struct {
	mu      sync.Mutex
	grant   mcprelay.Grant
	err     error
	digests []mcprelay.CapabilityDigest
}

func (v *testGrantVerifier) VerifyMCPGrant(_ context.Context, digest mcprelay.CapabilityDigest) (mcprelay.Grant, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.digests = append(v.digests, digest)
	return v.grant, v.err
}

type testLifecycleStore struct {
	lifecycle mcprelay.InstanceLifecycle
}

func (s *testLifecycleStore) MCPInstanceLifecycle(context.Context, string) (mcprelay.InstanceLifecycle, error) {
	return s.lifecycle, nil
}

type testUpstream struct {
	mu         sync.Mutex
	tools      []*mcp.Tool
	callResult *mcp.CallToolResult
	listErr    error
	callErr    error
	targets    []mcprelay.UpstreamTarget
	calls      []testCall
}

type testCall struct {
	name      string
	arguments json.RawMessage
}

func (u *testUpstream) ListTools(_ context.Context, target mcprelay.UpstreamTarget, yield func(mcprelay.ToolPage) error) error {
	u.mu.Lock()
	u.targets = append(u.targets, target)
	tools := slices.Clone(u.tools)
	err := u.listErr
	u.mu.Unlock()
	if err != nil {
		return err
	}
	return yield(mcprelay.ToolPage{Tools: tools})
}

func (u *testUpstream) CallTool(
	_ context.Context,
	target mcprelay.UpstreamTarget,
	name string,
	arguments json.RawMessage,
) (*mcp.CallToolResult, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.targets = append(u.targets, target)
	u.calls = append(u.calls, testCall{name: name, arguments: slices.Clone(arguments)})
	return u.callResult, u.callErr
}

type transportFixture struct {
	capability string
	binding    translator.MCPPolicyBinding
	grants     *testGrantVerifier
	upstream   *testUpstream
	handler    *Handler
}

func newTransportFixture(t *testing.T) *transportFixture {
	t.Helper()
	binding := relayTestBinding("read")
	capability := strings.Repeat("c", 43)
	grants := &testGrantVerifier{grant: mcprelay.Grant{
		AgentInstanceID: "instance-1",
		Revision:        "revision-1",
		BindingID:       binding.ID,
		ExpiresAt:       relayTestNow.Add(time.Hour),
	}}
	upstream := &testUpstream{
		tools: []*mcp.Tool{{
			Name:        "read",
			Description: "Read an item",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}}}`),
		}},
		callResult: &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}},
	}
	engine, err := mcprelay.New(mcprelay.Config{
		Policies: &testPolicyStore{policy: translator.MCPPolicyV1{
			Version: translator.MCPPolicyVersionV1, Bindings: []translator.MCPPolicyBinding{binding},
		}},
		Grants: grants,
		Lifecycles: &testLifecycleStore{lifecycle: mcprelay.InstanceLifecycle{
			AgentInstanceID: "instance-1", PreparedRevision: "revision-1", State: mcprelay.InstanceStateReady,
		}},
		Upstream: upstream,
		Now:      func() time.Time { return relayTestNow },
	})
	require.NoError(t, err)
	handler, err := New(engine)
	require.NoError(t, err)
	return &transportFixture{
		capability: capability, binding: binding, grants: grants, upstream: upstream, handler: handler,
	}
}

func relayTestBinding(tools ...string) translator.MCPPolicyBinding {
	tools = slices.Clone(tools)
	slices.Sort(tools)
	binding := translator.MCPPolicyBinding{
		SubjectPath: []string{"root"},
		Server: translator.MCPServerIdentity{
			Namespace: "agents", Name: "knowledge", UID: "uid-knowledge",
			SpecHash: strings.Repeat("a", sha256.Size*2),
		},
		Tools: tools,
	}
	raw, err := json.Marshal(struct {
		SubjectPath []string                     `json:"subjectPath"`
		Server      translator.MCPServerIdentity `json:"server"`
		Tools       []string                     `json:"tools"`
	}{binding.SubjectPath, binding.Server, binding.Tools})
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(raw)
	binding.ID = "mcp-" + hex.EncodeToString(digest[:])
	return binding
}

func (f *transportFixture) endpoint(server *httptest.Server) string {
	return server.URL + "/internal/v1/mcp-relay/bindings/" + f.binding.ID + "/mcp"
}

func TestOfficialMCPClientListAndCall(t *testing.T) {
	f := newTransportFixture(t)
	httpServer := httptest.NewServer(f.handler)
	t.Cleanup(httpServer.Close)

	httpClient := &http.Client{Transport: &bearerTransport{capability: f.capability}}
	client := mcp.NewClient(&mcp.Implementation{Name: "relay-test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: f.endpoint(httpServer), HTTPClient: httpClient, DisableStandaloneSSE: true,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })

	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, "private", listed.CacheScope)
	require.Len(t, listed.Tools, 1)
	require.Equal(t, "read", listed.Tools[0].Name)

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "read", Arguments: map[string]any{"id": "42"},
	})
	require.NoError(t, err)
	require.Equal(t, "ok", result.Content[0].(*mcp.TextContent).Text)
	f.grants.mu.Lock()
	require.NotEmpty(t, f.grants.digests)
	wantDigest := mcprelay.CapabilityDigest(sha256.Sum256([]byte(f.capability)))
	for _, digest := range f.grants.digests {
		require.Equal(t, wantDigest, digest)
	}
	f.grants.mu.Unlock()

	f.upstream.mu.Lock()
	defer f.upstream.mu.Unlock()
	require.Len(t, f.upstream.calls, 1)
	require.JSONEq(t, `{"id":"42"}`, string(f.upstream.calls[0].arguments))
	for _, target := range f.upstream.targets {
		require.Equal(t, f.binding.ID, target.BindingID)
		require.Equal(t, f.binding.Server, target.Server)
	}
}

type bearerTransport struct {
	capability string
}

func (t *bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	forwarded := request.Clone(request.Context())
	forwarded.Header = request.Header.Clone()
	forwarded.Header.Set("Authorization", "Bearer "+t.capability)
	return http.DefaultTransport.RoundTrip(forwarded)
}

func TestInitializeUsesOfficialStreamableHTTPProtocol(t *testing.T) {
	f := newTransportFixture(t)
	request := relayRequest(t, http.MethodPost, relayPath(f.binding.ID), f.capability, `{
		"jsonrpc":"2.0","id":1,"method":"initialize","params":{
			"protocolVersion":"2025-03-26","capabilities":{},
			"clientInfo":{"name":"test","version":"1"}
		}
	}`)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "application/json", response.Header().Get("Content-Type"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	result := body["result"].(map[string]any)
	require.Equal(t, "2025-03-26", result["protocolVersion"])
	require.Equal(t, "kagent-mcp-relay", result["serverInfo"].(map[string]any)["name"])
}

func TestAuthorizationIsRemovedBeforeMCPHandling(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 43))
	request.Header.Set("MCP-Protocol-Version", "2025-03-26")

	forwarded := withoutAuthorization(request)
	require.Empty(t, forwarded.Header.Values("Authorization"))
	require.Equal(t, "2025-03-26", forwarded.Header.Get("MCP-Protocol-Version"))
	require.NotEmpty(t, request.Header.Values("Authorization"), "the caller-owned request must not be mutated")
}

func TestHTTPBoundaryRejectsInvalidRequestsBeforeEngine(t *testing.T) {
	f := newTransportFixture(t)
	validBody := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	tests := []struct {
		name       string
		request    func(*testing.T) *http.Request
		wantStatus int
		wantAllow  string
	}{
		{
			name: "health is not served on relay listener",
			request: func(t *testing.T) *http.Request {
				return relayRequest(t, http.MethodGet, "/health", f.capability, "")
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "method",
			request: func(t *testing.T) *http.Request {
				return relayRequest(t, http.MethodGet, relayPath(f.binding.ID), f.capability, "")
			},
			wantStatus: http.StatusMethodNotAllowed, wantAllow: http.MethodPost,
		},
		{
			name: "query",
			request: func(t *testing.T) *http.Request {
				return relayRequest(t, http.MethodPost, relayPath(f.binding.ID)+"?capability="+f.capability, "", validBody)
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "invalid binding",
			request: func(t *testing.T) *http.Request {
				return relayRequest(t, http.MethodPost, relayPath("mcp-not-a-digest"), f.capability, validBody)
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "missing authorization",
			request: func(t *testing.T) *http.Request {
				return relayRequest(t, http.MethodPost, relayPath(f.binding.ID), "", validBody)
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "malformed authorization",
			request: func(t *testing.T) *http.Request {
				request := relayRequest(t, http.MethodPost, relayPath(f.binding.ID), "", validBody)
				request.Header.Set("Authorization", "Bearer short secret")
				return request
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "duplicate authorization",
			request: func(t *testing.T) *http.Request {
				request := relayRequest(t, http.MethodPost, relayPath(f.binding.ID), f.capability, validBody)
				request.Header.Add("Authorization", "Bearer "+f.capability)
				return request
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "content type",
			request: func(t *testing.T) *http.Request {
				request := relayRequest(t, http.MethodPost, relayPath(f.binding.ID), f.capability, validBody)
				request.Header.Set("Content-Type", "text/plain")
				return request
			},
			wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name: "duplicate content type",
			request: func(t *testing.T) *http.Request {
				request := relayRequest(t, http.MethodPost, relayPath(f.binding.ID), f.capability, validBody)
				request.Header.Add("Content-Type", "application/json")
				return request
			},
			wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name: "accept",
			request: func(t *testing.T) *http.Request {
				request := relayRequest(t, http.MethodPost, relayPath(f.binding.ID), f.capability, validBody)
				request.Header.Set("Accept", "application/json")
				return request
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "body bound",
			request: func(t *testing.T) *http.Request {
				return relayRequest(t, http.MethodPost, relayPath(f.binding.ID), f.capability, strings.Repeat("x", maxRequestBodyBytes+1))
			},
			wantStatus: http.StatusRequestEntityTooLarge,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			f.handler.ServeHTTP(response, test.request(t))
			require.Equal(t, test.wantStatus, response.Code, response.Body.String())
			require.Equal(t, test.wantAllow, response.Header().Get("Allow"))
			require.NotContains(t, response.Body.String(), f.capability)
		})
	}

	f.grants.mu.Lock()
	defer f.grants.mu.Unlock()
	require.Empty(t, f.grants.digests)
}

func TestRouteBindingCannotBeOverriddenByToolArguments(t *testing.T) {
	f := newTransportFixture(t)
	other := relayTestBinding("write")
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"bindingID":"` + f.binding.ID + `"}}}`
	request := relayRequest(t, http.MethodPost, relayPath(other.ID), f.capability, body)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Body.String(), "relay operation is not permitted")
	require.NotContains(t, response.Body.String(), f.capability)

	f.upstream.mu.Lock()
	defer f.upstream.mu.Unlock()
	require.Empty(t, f.upstream.calls)
	require.Empty(t, f.upstream.targets)
}

func TestDependencyErrorsAreRedacted(t *testing.T) {
	f := newTransportFixture(t)
	f.grants.err = errors.New("postgres://admin:password@database.internal")
	request := relayRequest(t, http.MethodPost, relayPath(f.binding.ID), f.capability,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Body.String(), "relay authentication failed")
	require.NotContains(t, response.Body.String(), "postgres")
	require.NotContains(t, response.Body.String(), "password")
	require.NotContains(t, response.Body.String(), f.capability)
}

func TestUnsupportedMCPMethodIsRejected(t *testing.T) {
	f := newTransportFixture(t)
	request := relayRequest(t, http.MethodPost, relayPath(f.binding.ID), f.capability,
		`{"jsonrpc":"2.0","id":1,"method":"resources/list","params":{}}`)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Body.String(), `"code":-32601`)
	require.NotContains(t, response.Body.String(), f.capability)
}

func relayPath(bindingID string) string {
	return "/internal/v1/mcp-relay/bindings/" + bindingID + "/mcp"
}

func relayRequest(t *testing.T, method, path, capability, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if capability != "" {
		request.Header.Set("Authorization", "Bearer "+capability)
	}
	return request
}
