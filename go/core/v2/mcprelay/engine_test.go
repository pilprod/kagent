package mcprelay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/core/v2/translator"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

const (
	testInstanceID = "instance-1"
	testRevision   = "revision-1"
)

var testNow = time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC)

type fakePolicyStore struct {
	policy    translator.MCPPolicyV1
	err       error
	revisions []string
	load      func(context.Context, string) (translator.MCPPolicyV1, error)
}

func (s *fakePolicyStore) MCPPolicy(ctx context.Context, revision string) (translator.MCPPolicyV1, error) {
	s.revisions = append(s.revisions, revision)
	if s.load != nil {
		return s.load(ctx, revision)
	}
	return s.policy, s.err
}

type fakeGrantVerifier struct {
	grant   Grant
	err     error
	digests []CapabilityDigest
	verify  func(context.Context, CapabilityDigest) (Grant, error)
}

func (v *fakeGrantVerifier) VerifyMCPGrant(ctx context.Context, digest CapabilityDigest) (Grant, error) {
	v.digests = append(v.digests, digest)
	if v.verify != nil {
		return v.verify(ctx, digest)
	}
	return v.grant, v.err
}

type fakeLifecycleStore struct {
	lifecycle InstanceLifecycle
	err       error
	ids       []string
	load      func(context.Context, string) (InstanceLifecycle, error)
}

func (s *fakeLifecycleStore) MCPInstanceLifecycle(ctx context.Context, id string) (InstanceLifecycle, error) {
	s.ids = append(s.ids, id)
	if s.load != nil {
		return s.load(ctx, id)
	}
	return s.lifecycle, s.err
}

type listInvocation struct {
	target UpstreamTarget
	cursor string
}

type callInvocation struct {
	target    UpstreamTarget
	tool      string
	arguments json.RawMessage
}

type fakeUpstream struct {
	pages      map[string]ToolPage
	listErr    error
	callResult *mcp.CallToolResult
	callErr    error
	lists      []listInvocation
	calls      []callInvocation
}

func (u *fakeUpstream) ListTools(_ context.Context, target UpstreamTarget, cursor string) (ToolPage, error) {
	u.lists = append(u.lists, listInvocation{target: target, cursor: cursor})
	return u.pages[cursor], u.listErr
}

func (u *fakeUpstream) CallTool(
	_ context.Context,
	target UpstreamTarget,
	tool string,
	arguments json.RawMessage,
) (*mcp.CallToolResult, error) {
	u.calls = append(u.calls, callInvocation{target: target, tool: tool, arguments: slices.Clone(arguments)})
	return u.callResult, u.callErr
}

type fixture struct {
	capability string
	binding    translator.MCPPolicyBinding
	policies   *fakePolicyStore
	grants     *fakeGrantVerifier
	lifecycles *fakeLifecycleStore
	upstream   *fakeUpstream
	engine     *Engine
}

func newFixture(t *testing.T, tools ...string) *fixture {
	t.Helper()
	binding := testBinding("knowledge", tools...)
	policies := &fakePolicyStore{policy: testPolicy(binding)}
	grants := &fakeGrantVerifier{grant: Grant{
		AgentInstanceID: testInstanceID, Revision: testRevision, BindingID: binding.ID,
		ExpiresAt: testNow.Add(10 * time.Minute),
	}}
	lifecycles := &fakeLifecycleStore{lifecycle: InstanceLifecycle{
		AgentInstanceID: testInstanceID, PreparedRevision: testRevision, State: InstanceStateReady,
	}}
	upstream := &fakeUpstream{pages: map[string]ToolPage{}, callResult: &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "ok"}},
	}}
	engine, err := New(Config{
		Policies: policies, Grants: grants, Lifecycles: lifecycles, Upstream: upstream,
		Now: func() time.Time { return testNow },
	})
	require.NoError(t, err)
	return &fixture{
		capability: strings.Repeat("c", 43), binding: binding, policies: policies,
		grants: grants, lifecycles: lifecycles, upstream: upstream, engine: engine,
	}
}

func testPolicy(bindings ...translator.MCPPolicyBinding) translator.MCPPolicyV1 {
	slices.SortFunc(bindings, func(a, b translator.MCPPolicyBinding) int { return strings.Compare(a.ID, b.ID) })
	return translator.MCPPolicyV1{Version: translator.MCPPolicyVersionV1, Bindings: bindings}
}

func testBinding(serverName string, tools ...string) translator.MCPPolicyBinding {
	tools = slices.Clone(tools)
	slices.Sort(tools)
	tools = slices.Compact(tools)
	binding := translator.MCPPolicyBinding{
		SubjectPath: []string{"root"},
		Server: translator.MCPServerIdentity{
			Namespace: "agents", Name: serverName, UID: "uid-" + serverName,
			SpecHash: strings.Repeat("a", sha256.Size*2),
		},
		Tools: tools,
	}
	raw, err := json.Marshal(struct {
		SubjectPath []string                     `json:"subjectPath"`
		Server      translator.MCPServerIdentity `json:"server"`
		Tools       []string                     `json:"tools"`
	}{SubjectPath: binding.SubjectPath, Server: binding.Server, Tools: binding.Tools})
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(raw)
	binding.ID = "mcp-" + hex.EncodeToString(digest[:])
	return binding
}

func validTool(name string) *mcp.Tool {
	return &mcp.Tool{
		Name: name, Description: "Tool " + name,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func TestListToolsFiltersAndConsumesUpstreamPages(t *testing.T) {
	f := newFixture(t, "beta", "alpha")
	unselected := &mcp.Tool{
		Name: "admin", Description: "Bearer secret from https://cluster.internal",
		InputSchema: json.RawMessage(`{"type":"object","type":"array"}`),
	}
	f.upstream.pages = map[string]ToolPage{
		"":          {Tools: []*mcp.Tool{validTool("alpha"), unselected}, NextCursor: "next-page"},
		"next-page": {Tools: []*mcp.Tool{validTool("beta")}},
	}

	tools, err := f.engine.ListTools(context.Background(), f.capability, f.binding.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "beta"}, []string{tools[0].Name, tools[1].Name})
	raw, err := json.Marshal(tools)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "admin")
	require.NotContains(t, string(raw), "cluster.internal")
	require.Equal(t, []string{"", "next-page"}, []string{f.upstream.lists[0].cursor, f.upstream.lists[1].cursor})
	for _, call := range f.upstream.lists {
		require.Equal(t, UpstreamTarget{
			AgentInstanceID: testInstanceID,
			Revision:        testRevision,
			BindingID:       f.binding.ID,
			Server:          f.binding.Server,
		}, call.target)
	}
	require.Equal(t, CapabilityDigest(sha256.Sum256([]byte(f.capability))), f.grants.digests[0])
}

func TestCallToolAuthorizesBeforeUpstream(t *testing.T) {
	tests := []struct {
		name      string
		bindingID func(*fixture) string
		tool      string
		mutate    func(*fixture)
		want      error
	}{
		{
			name: "arbitrary binding", tool: "read", want: ErrPermissionDenied,
			bindingID: func(*fixture) string { return "mcp-" + strings.Repeat("f", 64) },
		},
		{name: "tool outside allowlist", tool: "delete", want: ErrPermissionDenied},
		{
			name: "stale revision", tool: "read", want: ErrPermissionDenied,
			mutate: func(f *fixture) { f.lifecycles.lifecycle.PreparedRevision = "revision-2" },
		},
		{
			name: "expired", tool: "read", want: ErrUnauthenticated,
			mutate: func(f *fixture) { f.grants.grant.ExpiresAt = testNow },
		},
		{
			name: "revoked", tool: "read", want: ErrUnauthenticated,
			mutate: func(f *fixture) { revoked := testNow.Add(-time.Minute); f.grants.grant.RevokedAt = &revoked },
		},
		{
			name: "suspended", tool: "read", want: ErrPermissionDenied,
			mutate: func(f *fixture) { f.lifecycles.lifecycle.State = InstanceStateSuspended },
		},
		{
			name: "lifecycle operation", tool: "read", want: ErrPermissionDenied,
			mutate: func(f *fixture) { f.lifecycles.lifecycle.OperationPending = true },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, "read")
			if test.mutate != nil {
				test.mutate(f)
			}
			bindingID := f.binding.ID
			if test.bindingID != nil {
				bindingID = test.bindingID(f)
			}
			_, err := f.engine.CallTool(context.Background(), f.capability, bindingID, test.tool, json.RawMessage(`{}`))
			require.ErrorIs(t, err, test.want)
			require.Empty(t, f.upstream.calls)
			require.Empty(t, f.upstream.lists)
		})
	}
}

func TestCallToolDerivesPinnedUpstreamTargetAndSelectedTool(t *testing.T) {
	f := newFixture(t, "read")
	result, err := f.engine.CallTool(context.Background(), f.capability, f.binding.ID, "read", json.RawMessage(`{"id":"42"}`))
	require.NoError(t, err)
	require.Equal(t, "ok", result.Content[0].(*mcp.TextContent).Text)
	require.Len(t, f.upstream.calls, 1)
	require.Equal(t, UpstreamTarget{
		AgentInstanceID: testInstanceID,
		Revision:        testRevision,
		BindingID:       f.binding.ID,
		Server:          f.binding.Server,
	}, f.upstream.calls[0].target)
	require.Equal(t, "read", f.upstream.calls[0].tool)
	require.JSONEq(t, `{"id":"42"}`, string(f.upstream.calls[0].arguments))
}

func TestCallToolDetachesValidatedResultAndDropsHiddenState(t *testing.T) {
	f := newFixture(t, "read")
	structured := map[string]any{"items": []any{"original"}}
	contentMeta := mcp.Meta{"nested": map[string]any{"value": "original"}}
	text := &mcp.TextContent{Text: "original", Meta: contentMeta}
	upstreamResult := &mcp.CallToolResult{
		Meta:              mcp.Meta{"source": map[string]any{"value": "original"}},
		StructuredContent: structured,
	}
	upstreamResult.SetError(errors.New("hidden Authorization: Bearer secret"))
	upstreamResult.Content = []mcp.Content{text}
	f.upstream.callResult = upstreamResult

	result, err := f.engine.CallTool(context.Background(), f.capability, f.binding.ID, "read", json.RawMessage(`{}`))
	require.NoError(t, err)
	require.NotSame(t, upstreamResult, result)
	require.True(t, result.IsError)
	require.NoError(t, result.GetError())

	// Mutating every reachable upstream value after validation must not change
	// the result returned across the relay trust boundary.
	text.Text = "mutated"
	contentMeta["nested"].(map[string]any)["value"] = "mutated"
	structured["items"].([]any)[0] = "mutated"
	upstreamResult.Meta["source"].(map[string]any)["value"] = "mutated"
	upstreamResult.Content = nil

	detachedText := result.Content[0].(*mcp.TextContent)
	require.Equal(t, "original", detachedText.Text)
	require.Equal(t, "original", detachedText.Meta["nested"].(map[string]any)["value"])
	require.Equal(t, "original", result.StructuredContent.(map[string]any)["items"].([]any)[0])
	require.Equal(t, "original", result.Meta["source"].(map[string]any)["value"])
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "Bearer secret")
}

func TestCallToolResultContentWhitelist(t *testing.T) {
	t.Run("allows tools call content", func(t *testing.T) {
		f := newFixture(t, "read")
		size := int64(3)
		f.upstream.callResult = &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: "ok"},
			&mcp.ImageContent{Data: []byte("img"), MIMEType: "image/png"},
			&mcp.AudioContent{Data: []byte("audio"), MIMEType: "audio/wav"},
			&mcp.ResourceLink{URI: "https://example.invalid/resource", Name: "resource", Size: &size},
			&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "file:///resource", Text: "value"}},
		}}

		result, err := f.engine.CallTool(context.Background(), f.capability, f.binding.ID, "read", json.RawMessage(`{}`))
		require.NoError(t, err)
		require.IsType(t, &mcp.TextContent{}, result.Content[0])
		require.IsType(t, &mcp.ImageContent{}, result.Content[1])
		require.IsType(t, &mcp.AudioContent{}, result.Content[2])
		require.IsType(t, &mcp.ResourceLink{}, result.Content[3])
		require.IsType(t, &mcp.EmbeddedResource{}, result.Content[4])
	})

	// These deprecated sampling-only blocks remain in the SDK during its
	// compatibility window and are precisely the types the relay must reject.
	toolUse := &mcp.ToolUseContent{ //nolint:staticcheck // Regression coverage for forbidden sampling-only content.
		ID: "call-1", Name: "read", Input: map[string]any{},
	}
	toolResult := &mcp.ToolResultContent{ //nolint:staticcheck // Regression coverage for forbidden sampling-only content.
		ToolUseID: "call-1", Content: []mcp.Content{&mcp.TextContent{Text: "nested"}},
	}
	var nilText *mcp.TextContent
	for _, test := range []struct {
		name    string
		content mcp.Content
	}{
		{name: "tool use", content: toolUse},
		{name: "tool result", content: toolResult},
		{name: "typed nil", content: nilText},
	} {
		t.Run("rejects "+test.name, func(t *testing.T) {
			f := newFixture(t, "read")
			f.upstream.callResult = &mcp.CallToolResult{Content: []mcp.Content{test.content}}
			_, err := f.engine.CallTool(context.Background(), f.capability, f.binding.ID, "read", json.RawMessage(`{}`))
			require.ErrorIs(t, err, ErrUpstream)
		})
	}
}

func TestListToolsFailsClosedOnMalformedUpstream(t *testing.T) {
	tests := []struct {
		name  string
		pages map[string]ToolPage
	}{
		{name: "selected tool missing", pages: map[string]ToolPage{"": {Tools: []*mcp.Tool{validTool("other")}}}},
		{name: "selected tool duplicated", pages: map[string]ToolPage{
			"":      {Tools: []*mcp.Tool{validTool("read")}, NextCursor: "again"},
			"again": {Tools: []*mcp.Tool{validTool("read")}},
		}},
		{name: "cursor cycle", pages: map[string]ToolPage{
			"":      {Tools: []*mcp.Tool{}, NextCursor: "again"},
			"again": {Tools: []*mcp.Tool{}, NextCursor: "again"},
		}},
		{name: "duplicate schema key", pages: map[string]ToolPage{"": {Tools: []*mcp.Tool{{
			Name: "read", InputSchema: json.RawMessage(`{"type":"object","type":"array"}`),
		}}}}},
		{name: "wrong schema root", pages: map[string]ToolPage{"": {Tools: []*mcp.Tool{{
			Name: "read", InputSchema: map[string]any{"type": "array"},
		}}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, "read")
			f.upstream.pages = test.pages
			_, err := f.engine.ListTools(context.Background(), f.capability, f.binding.ID)
			require.ErrorIs(t, err, ErrUpstream)
		})
	}
}

func TestListToolsEnforcesUpstreamBounds(t *testing.T) {
	t.Run("schema size", func(t *testing.T) {
		f := newFixture(t, "read")
		f.upstream.pages[""] = ToolPage{Tools: []*mcp.Tool{{
			Name:        "read",
			InputSchema: json.RawMessage(`{"type":"object","description":"` + strings.Repeat("x", maxSchemaBytes) + `"}`),
		}}}
		_, err := f.engine.ListTools(context.Background(), f.capability, f.binding.ID)
		require.ErrorIs(t, err, ErrUpstream)
	})
	t.Run("description size", func(t *testing.T) {
		f := newFixture(t, "read")
		tool := validTool("read")
		tool.Description = strings.Repeat("x", maxDescriptionBytes+1)
		f.upstream.pages[""] = ToolPage{Tools: []*mcp.Tool{tool}}
		_, err := f.engine.ListTools(context.Background(), f.capability, f.binding.ID)
		require.ErrorIs(t, err, ErrUpstream)
	})
	t.Run("page item count", func(t *testing.T) {
		f := newFixture(t, "read")
		f.upstream.pages[""] = ToolPage{Tools: make([]*mcp.Tool, maxToolsPerPage+1)}
		_, err := f.engine.ListTools(context.Background(), f.capability, f.binding.ID)
		require.ErrorIs(t, err, ErrUpstream)
	})
	t.Run("output schema root", func(t *testing.T) {
		f := newFixture(t, "read")
		tool := validTool("read")
		tool.OutputSchema = []any{"not", "a", "schema"}
		f.upstream.pages[""] = ToolPage{Tools: []*mcp.Tool{tool}}
		_, err := f.engine.ListTools(context.Background(), f.capability, f.binding.ID)
		require.ErrorIs(t, err, ErrUpstream)
	})
}

func TestListToolsAcceptsOpaqueUnicodeWhitespaceCursor(t *testing.T) {
	f := newFixture(t, "read")
	cursor := " \u2003страница α\u00a0 "
	f.upstream.pages = map[string]ToolPage{
		"":     {NextCursor: cursor},
		cursor: {Tools: []*mcp.Tool{validTool("read")}},
	}

	tools, err := f.engine.ListTools(context.Background(), f.capability, f.binding.ID)
	require.NoError(t, err)
	require.Equal(t, "read", tools[0].Name)
	require.Equal(t, cursor, f.upstream.lists[1].cursor)
}

func TestListToolsRejectsInvalidOpaqueCursor(t *testing.T) {
	for _, test := range []struct {
		name   string
		cursor string
	}{
		{name: "invalid UTF-8", cursor: string([]byte{0xff})},
		{name: "NUL", cursor: "next\x00page"},
		{name: "tab", cursor: "next\tpage"},
		{name: "newline", cursor: "next\npage"},
		{name: "escape", cursor: "next\x1bpage"},
		{name: "oversized", cursor: strings.Repeat("x", maxPaginationCursor+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, "read")
			f.upstream.pages[""] = ToolPage{NextCursor: test.cursor}
			_, err := f.engine.ListTools(context.Background(), f.capability, f.binding.ID)
			require.ErrorIs(t, err, ErrUpstream)
			require.Len(t, f.upstream.lists, 1)
		})
	}
}

func TestRelayRejectsMalformedArgumentsBeforeUpstream(t *testing.T) {
	tests := []struct {
		name      string
		arguments json.RawMessage
	}{
		{name: "not object", arguments: json.RawMessage(`[]`)},
		{name: "duplicate key", arguments: json.RawMessage(`{"id":1,"id":2}`)},
		{name: "invalid JSON", arguments: json.RawMessage(`{"id":`)},
		{name: "oversized", arguments: json.RawMessage(`{"value":"` + strings.Repeat("x", maxArgumentsBytes) + `"}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, "read")
			_, err := f.engine.CallTool(context.Background(), f.capability, f.binding.ID, "read", test.arguments)
			require.ErrorIs(t, err, ErrInvalidRequest)
			require.Empty(t, f.upstream.calls)
		})
	}
}

func TestDuplicateJSONErrorDoesNotReflectLongKey(t *testing.T) {
	f := newFixture(t, "read")
	key := "sensitive-token-" + strings.Repeat("x", 32<<10)
	arguments := json.RawMessage(`{"` + key + `":1,"` + key + `":2}`)

	_, err := f.engine.CallTool(context.Background(), f.capability, f.binding.ID, "read", arguments)
	require.ErrorIs(t, err, ErrInvalidRequest)
	require.NotContains(t, err.Error(), "sensitive-token")
	require.Less(t, len(err.Error()), 256)
	require.Empty(t, f.upstream.calls)
}

func TestRelaySanitizesDependencyErrors(t *testing.T) {
	dependencyError := errors.New("dial https://cluster.internal with Authorization: Bearer secret")
	for _, test := range []struct {
		name   string
		kind   error
		mutate func(*fixture)
	}{
		{name: "grant", kind: ErrUnauthenticated, mutate: func(f *fixture) { f.grants.err = dependencyError }},
		{name: "lifecycle", kind: ErrUnavailable, mutate: func(f *fixture) { f.lifecycles.err = dependencyError }},
		{name: "policy", kind: ErrUnavailable, mutate: func(f *fixture) { f.policies.err = dependencyError }},
		{name: "upstream", kind: ErrUpstream, mutate: func(f *fixture) { f.upstream.callErr = dependencyError }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, "read")
			test.mutate(f)
			_, err := f.engine.CallTool(context.Background(), f.capability, f.binding.ID, "read", json.RawMessage(`{}`))
			require.ErrorIs(t, err, test.kind)
			require.False(t, errors.Is(err, dependencyError))
			require.NotContains(t, err.Error(), "cluster.internal")
			require.NotContains(t, err.Error(), "Bearer secret")
			var internal interface{ Cause() error }
			require.False(t, errors.As(err, &internal))
		})
	}
}

func TestCallToolEnforcesResultSize(t *testing.T) {
	f := newFixture(t, "read")
	f.upstream.callResult = &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
		Text: strings.Repeat("x", maxCallResultBytes),
	}}}
	_, err := f.engine.CallTool(context.Background(), f.capability, f.binding.ID, "read", json.RawMessage(`{}`))
	require.ErrorIs(t, err, ErrUpstream)
}

type cancelingUpstream struct {
	entered chan struct{}
}

func (u *cancelingUpstream) ListTools(context.Context, UpstreamTarget, string) (ToolPage, error) {
	return ToolPage{}, errors.New("not used")
}

func (u *cancelingUpstream) CallTool(
	ctx context.Context,
	_ UpstreamTarget,
	_ string,
	_ json.RawMessage,
) (*mcp.CallToolResult, error) {
	close(u.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCallToolPropagatesCancellation(t *testing.T) {
	f := newFixture(t, "read")
	upstream := &cancelingUpstream{entered: make(chan struct{})}
	engine, err := New(Config{
		Policies: f.policies, Grants: f.grants, Lifecycles: f.lifecycles, Upstream: upstream,
		Now: func() time.Time { return testNow },
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := engine.CallTool(ctx, f.capability, f.binding.ID, "read", json.RawMessage(`{}`))
		done <- err
	}()
	<-upstream.entered
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestAuthorizationDependenciesPropagateCancellation(t *testing.T) {
	for _, test := range []struct {
		name    string
		install func(*fixture, context.CancelFunc)
	}{
		{
			name: "grant verifier",
			install: func(f *fixture, cancel context.CancelFunc) {
				f.grants.verify = func(context.Context, CapabilityDigest) (Grant, error) {
					cancel()
					return Grant{}, errors.New("grant dependency canceled")
				}
			},
		},
		{
			name: "lifecycle store",
			install: func(f *fixture, cancel context.CancelFunc) {
				f.lifecycles.load = func(context.Context, string) (InstanceLifecycle, error) {
					cancel()
					return InstanceLifecycle{}, errors.New("lifecycle dependency canceled")
				}
			},
		},
		{
			name: "policy store",
			install: func(f *fixture, cancel context.CancelFunc) {
				f.policies.load = func(context.Context, string) (translator.MCPPolicyV1, error) {
					cancel()
					return translator.MCPPolicyV1{}, errors.New("policy dependency canceled")
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, "read")
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			test.install(f, cancel)

			_, err := f.engine.CallTool(ctx, f.capability, f.binding.ID, "read", json.RawMessage(`{}`))
			require.ErrorIs(t, err, context.Canceled)
			require.Empty(t, f.upstream.calls)
			require.Empty(t, f.upstream.lists)
		})
	}
}

func TestRelayRejectsTamperedPersistedPolicy(t *testing.T) {
	f := newFixture(t, "read")
	f.policies.policy.Bindings[0].Server.Name = "arbitrary-server"
	_, err := f.engine.ListTools(context.Background(), f.capability, f.binding.ID)
	require.ErrorIs(t, err, ErrUnavailable)
	require.Empty(t, f.upstream.lists)
}
