package mcprelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/v2/translator"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type recordingRemoteExecutor struct {
	list       func(context.Context, *v1alpha3.RemoteMCPServer, func(*mcp.ListToolsResult) error) error
	call       func(context.Context, *v1alpha3.RemoteMCPServer, string, json.RawMessage) (*mcp.CallToolResult, error)
	pages      []*mcp.ListToolsResult
	listCalls  int
	callCalls  int
	lastServer *v1alpha3.RemoteMCPServer
	lastTool   string
	lastArgs   json.RawMessage
}

func (e *recordingRemoteExecutor) ListRemoteTools(
	ctx context.Context,
	server *v1alpha3.RemoteMCPServer,
	yield func(*mcp.ListToolsResult) error,
) error {
	e.listCalls++
	e.lastServer = server.DeepCopy()
	if e.list != nil {
		return e.list(ctx, server, yield)
	}
	for _, page := range e.pages {
		if err := yield(page); err != nil {
			return err
		}
	}
	return nil
}

func (e *recordingRemoteExecutor) CallRemoteTool(
	ctx context.Context,
	server *v1alpha3.RemoteMCPServer,
	toolName string,
	arguments json.RawMessage,
) (*mcp.CallToolResult, error) {
	e.callCalls++
	e.lastServer = server.DeepCopy()
	e.lastTool = toolName
	e.lastArgs = append(json.RawMessage(nil), arguments...)
	if e.call != nil {
		return e.call(ctx, server, toolName, arguments)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{}}, nil
}

func TestKubernetesUpstreamUsesOnlyPinnedClusterServer(t *testing.T) {
	server, target := pinnedServerFixture(t)
	kubeClient := relayKubeClient(t, server)
	executor := &recordingRemoteExecutor{
		pages: []*mcp.ListToolsResult{{
			Tools:      []*mcp.Tool{{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}},
			NextCursor: "next-page",
		}},
		call: func(context.Context, *v1alpha3.RemoteMCPServer, string, json.RawMessage) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		},
	}
	upstream := newKubernetesUpstream(kubeClient, executor)

	var page ToolPage
	err := upstream.ListTools(t.Context(), target, func(candidate ToolPage) error {
		page = candidate
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, "next-page", page.NextCursor)
	require.Len(t, page.Tools, 1)
	require.Equal(t, "read", page.Tools[0].Name)
	require.Equal(t, server.Spec.URL, executor.lastServer.Spec.URL)
	require.Equal(t, server.Spec.HeadersFrom, executor.lastServer.Spec.HeadersFrom)

	arguments := json.RawMessage(`{"id":"42"}`)
	result, err := upstream.CallTool(t.Context(), target, "read", arguments)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "read", executor.lastTool)
	require.JSONEq(t, string(arguments), string(executor.lastArgs))
	require.Equal(t, 1, executor.listCalls)
	require.Equal(t, 1, executor.callCalls)
}

func TestKubernetesUpstreamHidesConnectionConfigurationAndDependencyErrors(t *testing.T) {
	server, target := pinnedServerFixture(t)
	server.Spec.HeadersFrom = []v1alpha3.ValueRef{{
		Name:  "Authorization",
		Value: "Bearer private-header-value",
	}}
	target.Server.SpecHash = mustMCPServerSpecHash(t, server.Spec)
	executor := &recordingRemoteExecutor{
		list: func(context.Context, *v1alpha3.RemoteMCPServer, func(*mcp.ListToolsResult) error) error {
			return errors.New("dial https://cluster.internal with Authorization: Bearer private-header-value")
		},
	}
	upstream := newKubernetesUpstream(relayKubeClient(t, server), executor)

	publicTarget, err := json.Marshal(target)
	require.NoError(t, err)
	require.NotContains(t, string(publicTarget), server.Spec.URL)
	require.NotContains(t, string(publicTarget), "Authorization")
	require.NotContains(t, string(publicTarget), "private-header-value")

	err = upstream.ListTools(t.Context(), target, func(ToolPage) error { return nil })
	require.Error(t, err)
	require.NotContains(t, err.Error(), server.Spec.URL)
	require.NotContains(t, err.Error(), "Authorization")
	require.NotContains(t, err.Error(), "private-header-value")
}

func TestKubernetesUpstreamFailsClosedOnIdentityDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*v1alpha3.RemoteMCPServer, *UpstreamTarget) bool
	}{
		{
			name: "deleted",
			mutate: func(_ *v1alpha3.RemoteMCPServer, _ *UpstreamTarget) bool {
				return false
			},
		},
		{
			name: "recreated with a new UID",
			mutate: func(server *v1alpha3.RemoteMCPServer, _ *UpstreamTarget) bool {
				server.UID = k8stypes.UID("replacement-uid")
				return true
			},
		},
		{
			name: "specification changed",
			mutate: func(server *v1alpha3.RemoteMCPServer, _ *UpstreamTarget) bool {
				server.Spec.URL = "https://drift.example/mcp"
				return true
			},
		},
		{
			name: "deletion in progress",
			mutate: func(server *v1alpha3.RemoteMCPServer, _ *UpstreamTarget) bool {
				now := metav1.Now()
				server.DeletionTimestamp = &now
				server.Finalizers = []string{"relay.test/finalizer"}
				return true
			},
		},
		{
			name: "malformed specification hash",
			mutate: func(_ *v1alpha3.RemoteMCPServer, target *UpstreamTarget) bool {
				target.Server.SpecHash = "not-a-digest"
				return true
			},
		},
		{
			name: "TLS policy unsupported by executor",
			mutate: func(server *v1alpha3.RemoteMCPServer, target *UpstreamTarget) bool {
				server.Spec.TLS = &v1alpha3.TLSConfig{DisableVerify: true}
				target.Server.SpecHash = mustMCPServerSpecHash(t, server.Spec)
				return true
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, target := pinnedServerFixture(t)
			includeServer := test.mutate(server, &target)
			objects := []client.Object{}
			if includeServer {
				objects = append(objects, server)
			}
			executor := &recordingRemoteExecutor{}
			upstream := newKubernetesUpstream(relayKubeClient(t, objects...), executor)

			err := upstream.ListTools(t.Context(), target, func(ToolPage) error { return nil })
			require.Error(t, err)
			require.Zero(t, executor.listCalls)
			require.Zero(t, executor.callCalls)
		})
	}
}

func TestKubernetesUpstreamPreservesCancellation(t *testing.T) {
	server, target := pinnedServerFixture(t)
	entered := make(chan struct{})
	executor := &recordingRemoteExecutor{
		list: func(ctx context.Context, _ *v1alpha3.RemoteMCPServer, _ func(*mcp.ListToolsResult) error) error {
			close(entered)
			<-ctx.Done()
			return errors.New("dependency cancellation containing a private endpoint")
		},
	}
	upstream := newKubernetesUpstream(relayKubeClient(t, server), executor)
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- upstream.ListTools(ctx, target, func(ToolPage) error { return nil })
	}()

	<-entered
	cancel()
	require.ErrorIs(t, <-result, context.Canceled)
}

func TestNewKubernetesUpstreamRequiresBothClusterDependencies(t *testing.T) {
	kubeClient := relayKubeClient(t)
	upstream, err := NewKubernetesUpstream(nil, kubeClient)
	require.Error(t, err)
	require.Nil(t, upstream)

	upstream, err = NewKubernetesUpstream(kubeClient, nil)
	require.Error(t, err)
	require.Nil(t, upstream)
}

func TestKubernetesUpstreamResolvesHeaderSecretsInsideCluster(t *testing.T) {
	var issuedSessionIDs atomic.Int64
	mcpServer := mcp.NewServer(
		&mcp.Implementation{Name: "relay-test", Version: "v1"},
		&mcp.ServerOptions{
			PageSize: 1,
			GetSessionID: func() string {
				return fmt.Sprintf("session-%d", issuedSessionIDs.Add(1))
			},
		},
	)
	var argumentsMu sync.Mutex
	var receivedArguments string
	mcpServer.AddTool(
		&mcp.Tool{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			argumentsMu.Lock()
			receivedArguments = string(request.Params.Arguments)
			argumentsMu.Unlock()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		},
	)
	mcpServer.AddTool(
		&mcp.Tool{Name: "write", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "written"}}}, nil
		},
	)
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{JSONResponse: true},
	)
	var authenticatedRequests atomic.Int64
	var initializeRequests atomic.Int64
	var closedSessions atomic.Int64
	httpServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer cluster-secret" {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		authenticatedRequests.Add(1)
		if request.Method == http.MethodPost {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				http.Error(response, "invalid body", http.StatusBadRequest)
				return
			}
			request.Body = io.NopCloser(bytes.NewReader(body))
			var envelope struct {
				Method string `json:"method"`
			}
			if json.Unmarshal(body, &envelope) == nil && envelope.Method == "initialize" {
				initializeRequests.Add(1)
			}
		}
		if request.Method == http.MethodDelete {
			closedSessions.Add(1)
		}
		handler.ServeHTTP(response, request)
	}))
	t.Cleanup(httpServer.Close)

	server, target := pinnedServerFixture(t)
	server.Spec.URL = httpServer.URL
	server.Spec.HeadersFrom = []v1alpha3.ValueRef{{
		Name: "Authorization",
		ValueFrom: &v1alpha3.ValueSource{
			Type: v1alpha3.SecretValueSource,
			Name: "mcp-auth",
			Key:  "token",
		},
	}}
	target.Server.SpecHash = mustMCPServerSpecHash(t, server.Spec)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: server.Namespace, Name: "mcp-auth"},
		Data:       map[string][]byte{"token": []byte("Bearer cluster-secret")},
	}
	kubeClient := relayKubeClient(t, server, secret)
	upstream, err := NewKubernetesUpstream(kubeClient, kubeClient)
	require.NoError(t, err)

	pages := make([]ToolPage, 0, 2)
	err = upstream.ListTools(t.Context(), target, func(page ToolPage) error {
		pages = append(pages, page)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, pages, 2)
	require.Equal(t, "read", pages[0].Tools[0].Name)
	require.Equal(t, "write", pages[1].Tools[0].Name)
	require.Equal(t, int64(1), initializeRequests.Load(), "all list pages must share one MCP session")
	require.Equal(t, int64(1), closedSessions.Load(), "the list MCP session must be closed exactly once")

	arguments := json.RawMessage(`{"id":"42"}`)
	result, err := upstream.CallTool(t.Context(), target, "read", arguments)
	require.NoError(t, err)
	require.NotNil(t, result)
	argumentsMu.Lock()
	require.JSONEq(t, string(arguments), receivedArguments)
	argumentsMu.Unlock()
	require.Positive(t, authenticatedRequests.Load())
	require.Equal(t, int64(2), initializeRequests.Load(), "CallTool owns a separate scoped MCP session")
	require.Equal(t, int64(2), closedSessions.Load(), "CallTool must close its scoped MCP session")

	stop := errors.New("stop after the first validated page")
	callbackCount := 0
	err = upstream.ListTools(t.Context(), target, func(ToolPage) error {
		callbackCount++
		return stop
	})
	require.ErrorIs(t, err, stop)
	require.Equal(t, 1, callbackCount, "a callback error must stop pagination immediately")
	require.Equal(t, int64(3), initializeRequests.Load())
	require.Equal(t, int64(3), closedSessions.Load(), "callback failure must still close the scoped MCP session")
}

func pinnedServerFixture(t *testing.T) (*v1alpha3.RemoteMCPServer, UpstreamTarget) {
	t.Helper()
	server := &v1alpha3.RemoteMCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "agents",
			Name:      "files",
			UID:       k8stypes.UID("server-uid"),
		},
		Spec: v1alpha3.RemoteMCPServerSpec{
			Description: "Pinned files server",
			Protocol:    v1alpha3.RemoteMCPServerProtocolStreamableHttp,
			URL:         "https://cluster.internal/mcp",
		},
	}
	return server, UpstreamTarget{
		AgentInstanceID: "instance-1",
		Revision:        "revision-1",
		BindingID:       "mcp-binding-1",
		Server: translator.MCPServerIdentity{
			Namespace: server.Namespace,
			Name:      server.Name,
			UID:       string(server.UID),
			SpecHash:  mustMCPServerSpecHash(t, server.Spec),
		},
	}
}

func mustMCPServerSpecHash(t *testing.T, spec v1alpha3.RemoteMCPServerSpec) string {
	t.Helper()
	digest, err := translator.MCPServerSpecHash(spec)
	require.NoError(t, err)
	return digest
}

func relayKubeClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha3.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}
