// Package mcprelaytransport exposes the private, in-cluster MCP transport for
// revision-scoped runtime workers.
package mcprelaytransport

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"strings"

	"github.com/kagent-dev/kagent/go/core/internal/version"
	"github.com/kagent-dev/kagent/go/core/v2/mcprelay"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// RoutePattern is intentionally suitable only for a private controller
	// listener. No public Gateway, Ingress, or chart route is created here.
	RoutePattern = "POST /internal/v1/mcp-relay/bindings/{bindingID}/mcp"

	maxRequestBodyBytes = 2 << 20
	minCapabilityBytes  = 32
	maxCapabilityBytes  = 512
	bindingIDBytes      = len("mcp-") + 64
)

const (
	methodDiscover    = "server/discover"
	methodInitialize  = "initialize"
	methodInitialized = "notifications/initialized"
	methodPing        = "ping"
	methodListTools   = "tools/list"
	methodCallTool    = "tools/call"
)

const (
	codeUnauthenticated  = -32001
	codePermissionDenied = -32003
	codeUnavailable      = -32004
	codeUpstream         = -32005
)

// Handler is a strict, stateless Streamable HTTP adapter around Engine. It has
// no health endpoint and no fallback route: those remain on the controller's
// separate HTTP listener.
type Handler struct {
	engine *mcprelay.Engine
	routes *http.ServeMux
}

// New constructs the private relay handler.
func New(engine *mcprelay.Engine) (*Handler, error) {
	if engine == nil {
		return nil, errors.New("MCP relay engine is required")
	}
	h := &Handler{engine: engine, routes: http.NewServeMux()}
	h.routes.HandleFunc(RoutePattern, h.serveMCP)
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.routes.ServeHTTP(w, r)
}

func (h *Handler) serveMCP(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		http.Error(w, "query parameters are not accepted", http.StatusBadRequest)
		return
	}
	bindingID := r.PathValue("bindingID")
	if !validBindingID(bindingID) {
		http.NotFound(w, r)
		return
	}
	capability, ok := bearerCapability(r.Header.Values("Authorization"))
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "Bearer capability is required", http.StatusUnauthorized)
		return
	}
	if !jsonContentType(r.Header.Values("Content-Type")) {
		http.Error(w, "Content-Type must be 'application/json'", http.StatusUnsupportedMediaType)
		return
	}

	// Do not make the capability available to the MCP SDK, its RequestExtra
	// headers, or the upstream request context. The request-scoped MCP server
	// captures it only inside the authorization middleware below.
	forwarded := withoutAuthorization(r)

	server := h.server(capability, bindingID)
	mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless:                    true,
			JSONResponse:                 true,
			MaxRequestBodyBytes:          maxRequestBodyBytes,
			PropagateRequestCancellation: true,
		},
	).ServeHTTP(w, forwarded)
}

func (h *Handler) server(capability, bindingID string) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "kagent-mcp-relay", Version: version.Version},
		&mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{
			Tools: &mcp.ToolCapabilities{},
		}},
	)
	server.AddReceivingMiddleware(h.relayMiddleware(capability, bindingID))
	return server
}

func (h *Handler) relayMiddleware(capability, bindingID string) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			switch method {
			case methodListTools:
				typed, ok := request.(*mcp.ListToolsRequest)
				if !ok || typed.Params == nil {
					return nil, protocolError(jsonrpc.CodeInvalidParams, "invalid tools/list request")
				}
				if typed.Params.Cursor != "" {
					return nil, protocolError(jsonrpc.CodeInvalidParams, "relay tool pagination is not supported")
				}
				tools, err := h.engine.ListTools(ctx, capability, bindingID)
				if err != nil {
					return nil, relayError(err)
				}
				return &mcp.ListToolsResult{
					Cacheable: mcp.Cacheable{CacheScope: "private"},
					Tools:     tools,
				}, nil

			case methodCallTool:
				typed, ok := request.(*mcp.CallToolRequest)
				if !ok || typed.Params == nil {
					return nil, protocolError(jsonrpc.CodeInvalidParams, "invalid tools/call request")
				}
				if len(typed.Params.InputResponses) != 0 || typed.Params.RequestState != "" {
					return nil, protocolError(jsonrpc.CodeInvalidParams, "multi-round-trip tool calls are not supported")
				}
				result, err := h.engine.CallTool(ctx, capability, bindingID, typed.Params.Name, typed.Params.Arguments)
				if err != nil {
					return nil, relayError(err)
				}
				return result, nil

			case methodDiscover, methodInitialize, methodInitialized, methodPing:
				return next(ctx, method, request)
			default:
				return nil, protocolError(jsonrpc.CodeMethodNotFound, "method is not available on the MCP relay")
			}
		}
	}
}

func relayError(err error) error {
	switch {
	case errors.Is(err, mcprelay.ErrInvalidRequest):
		return protocolError(jsonrpc.CodeInvalidParams, "invalid relay request")
	case errors.Is(err, mcprelay.ErrUnauthenticated):
		return protocolError(codeUnauthenticated, "relay authentication failed")
	case errors.Is(err, mcprelay.ErrPermissionDenied):
		return protocolError(codePermissionDenied, "relay operation is not permitted")
	case errors.Is(err, mcprelay.ErrUnavailable):
		return protocolError(codeUnavailable, "relay authorization state is unavailable")
	case errors.Is(err, mcprelay.ErrUpstream):
		return protocolError(codeUpstream, "relay upstream is unavailable")
	default:
		return protocolError(jsonrpc.CodeInternalError, "relay request failed")
	}
}

func protocolError(code int64, message string) error {
	return &jsonrpc.Error{Code: code, Message: message}
}

func validBindingID(value string) bool {
	if len(value) != bindingIDBytes || !strings.HasPrefix(value, "mcp-") {
		return false
	}
	for _, character := range value[len("mcp-"):] {
		switch {
		case character >= '0' && character <= '9':
		case character >= 'a' && character <= 'f':
		default:
			return false
		}
	}
	return true
}

func bearerCapability(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	scheme, capability, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || len(capability) < minCapabilityBytes || len(capability) > maxCapabilityBytes {
		return "", false
	}
	padding := false
	for _, character := range capability {
		switch {
		case character == '=':
			padding = true
		case padding:
			return "", false
		case (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("-._~+/", character):
		default:
			return "", false
		}
	}
	return capability, true
}

func jsonContentType(values []string) bool {
	if len(values) != 1 {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(values[0])
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func withoutAuthorization(request *http.Request) *http.Request {
	forwarded := request.Clone(request.Context())
	forwarded.Header = request.Header.Clone()
	forwarded.Header.Del("Authorization")
	return forwarded
}
