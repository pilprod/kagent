package mcprelay

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/service/tool"
	"github.com/kagent-dev/kagent/go/core/v2/translator"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// remoteMCPExecutor consumes only a cluster-resolved RemoteMCPServer snapshot.
// It deliberately has no reference- or URL-based entry point.
type remoteMCPExecutor interface {
	ListRemoteTools(context.Context, *v1alpha3.RemoteMCPServer, func(*mcp.ListToolsResult) error) error
	CallRemoteTool(context.Context, *v1alpha3.RemoteMCPServer, string, json.RawMessage) (*mcp.CallToolResult, error)
}

// KubernetesUpstream resolves revision-pinned RemoteMCPServers and executes
// them through kagent's existing controller MCP client. Relay callers supply
// only UpstreamTarget; URL, headers, TLS settings, and Secret references are
// loaded inside the cluster trust boundary.
type KubernetesUpstream struct {
	serverReader client.Reader
	executor     remoteMCPExecutor
}

var (
	_ UpstreamClient    = (*KubernetesUpstream)(nil)
	_ remoteMCPExecutor = (*tool.RuntimeMCPClient)(nil)
)

// NewKubernetesUpstream constructs the in-cluster upstream. serverReader must
// be an authoritative API reader (for example manager.GetAPIReader), not an
// informer cache, so deletion and specification drift are observed at each
// invocation. kubeClient is used only by the existing MCP client to resolve
// referenced Secrets and establish the connection.
func NewKubernetesUpstream(serverReader client.Reader, kubeClient client.Client) (*KubernetesUpstream, error) {
	if serverReader == nil || kubeClient == nil {
		return nil, errors.New("authoritative RemoteMCPServer reader and Kubernetes client are required")
	}
	return newKubernetesUpstream(serverReader, tool.NewRuntimeMCPClient(kubeClient)), nil
}

func newKubernetesUpstream(serverReader client.Reader, executor remoteMCPExecutor) *KubernetesUpstream {
	return &KubernetesUpstream{serverReader: serverReader, executor: executor}
}

// ListTools resolves the exact pinned server once, then yields every tools page
// while one scoped upstream session remains open.
func (u *KubernetesUpstream) ListTools(
	ctx context.Context,
	target UpstreamTarget,
	yield func(ToolPage) error,
) error {
	if yield == nil {
		return errors.New("upstream tools page callback is required")
	}
	server, err := u.resolvePinnedServer(ctx, target)
	if err != nil {
		return err
	}
	var callbackErr error
	err = u.executor.ListRemoteTools(ctx, server, func(result *mcp.ListToolsResult) error {
		if result == nil {
			callbackErr = errors.New("pinned RemoteMCPServer returned an empty tools response")
			return callbackErr
		}
		callbackErr = yield(ToolPage{Tools: result.Tools, NextCursor: result.NextCursor})
		return callbackErr
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if callbackErr != nil {
		return callbackErr
	}
	if err != nil {
		return errors.New("failed to list pinned RemoteMCPServer tools")
	}
	return nil
}

// CallTool resolves the exact pinned server before invoking one tool.
func (u *KubernetesUpstream) CallTool(
	ctx context.Context,
	target UpstreamTarget,
	toolName string,
	arguments json.RawMessage,
) (*mcp.CallToolResult, error) {
	server, err := u.resolvePinnedServer(ctx, target)
	if err != nil {
		return nil, err
	}
	result, err := u.executor.CallRemoteTool(ctx, server, toolName, arguments)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, errors.New("failed to call pinned RemoteMCPServer tool")
	}
	if result == nil {
		return nil, errors.New("pinned RemoteMCPServer returned an empty tool result")
	}
	return result, nil
}

func (u *KubernetesUpstream) resolvePinnedServer(
	ctx context.Context,
	target UpstreamTarget,
) (*v1alpha3.RemoteMCPServer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateUpstreamTarget(target); err != nil {
		return nil, err
	}

	key := client.ObjectKey{Namespace: target.Server.Namespace, Name: target.Server.Name}
	server := &v1alpha3.RemoteMCPServer{}
	err := u.serverReader.Get(ctx, key, server)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errors.New("pinned RemoteMCPServer no longer exists")
		}
		return nil, fmt.Errorf("load pinned RemoteMCPServer: %w", err)
	}
	if server.DeletionTimestamp != nil {
		return nil, errors.New("pinned RemoteMCPServer is being deleted")
	}
	if server.Namespace != target.Server.Namespace || server.Name != target.Server.Name {
		return nil, errors.New("pinned RemoteMCPServer name drifted")
	}
	if string(server.UID) != target.Server.UID {
		return nil, errors.New("pinned RemoteMCPServer identity drifted")
	}
	specHash, err := translator.MCPServerSpecHash(server.Spec)
	if err != nil {
		return nil, fmt.Errorf("hash pinned RemoteMCPServer specification: %w", err)
	}
	if specHash != target.Server.SpecHash {
		return nil, errors.New("pinned RemoteMCPServer specification drifted")
	}

	// RuntimeMCPClient does not currently apply RemoteMCPServer TLSConfig. Do
	// not silently weaken a pinned trust policy by falling back to system roots.
	if !server.Spec.TLS.IsEmpty() {
		return nil, errors.New("pinned RemoteMCPServer TLS configuration is unsupported by the controller MCP client")
	}
	return server, nil
}

func validateUpstreamTarget(target UpstreamTarget) error {
	if target.AgentInstanceID == "" || target.Revision == "" || target.BindingID == "" {
		return errors.New("upstream authorization scope is incomplete")
	}
	if target.Server.Namespace == "" || target.Server.Name == "" || target.Server.UID == "" {
		return errors.New("pinned RemoteMCPServer identity is incomplete")
	}
	if len(target.Server.SpecHash) != 64 || strings.ToLower(target.Server.SpecHash) != target.Server.SpecHash {
		return errors.New("pinned RemoteMCPServer specification hash is invalid")
	}
	if _, err := hex.DecodeString(target.Server.SpecHash); err != nil {
		return errors.New("pinned RemoteMCPServer specification hash is invalid")
	}
	return nil
}
