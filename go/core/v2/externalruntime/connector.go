package externalruntime

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/v2/externalgateway"
	"github.com/kagent-dev/kagent/go/core/v2/runtimebackend"
)

const endpointHost = "external-runtime.invalid"

// Connector creates A2A clients backed by the in-cluster external runtime
// broker. It performs no connection retry or runtime lifecycle operation.
type Connector struct {
	broker *externalgateway.Broker
}

var _ runtimebackend.Connector = (*Connector)(nil)

// NewConnector constructs an external runtime connector.
func NewConnector(broker *externalgateway.Broker) (*Connector, error) {
	if broker == nil {
		return nil, fmt.Errorf("external runtime broker is nil")
	}
	return &Connector{broker: broker}, nil
}

// Dial validates the persisted slot authority and constructs an A2A 1.0
// JSON-RPC client for that slot's fixed invoke endpoint.
func (c *Connector) Dial(ctx context.Context, instance *apiv1alpha1.AgentInstance) (*a2aclient.Client, error) {
	if instance == nil {
		return nil, fmt.Errorf("external runtime connector requires an AgentInstance")
	}
	slot, err := DecodeAuthority(instance.GetA2AAuthority())
	if err != nil {
		return nil, fmt.Errorf("decode external runtime authority for AgentInstance %q: %w", instance.GetId(), err)
	}
	path, err := invokePath(slot.Runtime)
	if err != nil {
		return nil, err
	}

	httpClient := &http.Client{
		Transport: &brokerRoundTripper{broker: c.broker, slot: slot, invokePath: path},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	endpoint := a2atype.NewAgentInterface("https://"+endpointHost+path, a2atype.TransportProtocolJSONRPC)
	client, err := a2aclient.NewFromEndpoints(ctx, []*a2atype.AgentInterface{endpoint},
		a2aclient.WithDefaultsDisabled(),
		a2aclient.WithConfig(a2aclient.Config{DisableTenantPropagation: true}),
		a2aclient.WithJSONRPCTransport(httpClient),
	)
	if err != nil {
		return nil, fmt.Errorf("create external runtime A2A client for AgentInstance %q: %w", instance.GetId(), err)
	}
	return client, nil
}

func invokePath(runtime externalgateway.Runtime) (string, error) {
	switch runtime {
	case externalgateway.RuntimeCodex:
		return "/codex/v1/invoke", nil
	case externalgateway.RuntimeClaude:
		return "/claude/v1/invoke", nil
	default:
		return "", fmt.Errorf("external runtime ID is not supported: %w", ErrInvalidAuthority)
	}
}

type brokerRoundTripper struct {
	broker     *externalgateway.Broker
	slot       externalgateway.SlotKey
	invokePath string
}

var _ http.RoundTripper = (*brokerRoundTripper)(nil)

// RoundTrip intentionally makes exactly one broker call. A dispatched request
// with an unknown outcome must be resolved by the durable A2A task layer, never
// replayed here.
func (t *brokerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil {
		return nil, fmt.Errorf("external runtime HTTP request and URL are required: %w", externalgateway.ErrInvalidRequest)
	}
	if request.URL == nil {
		return nil, rejectRequest(request, fmt.Errorf("external runtime HTTP request and URL are required: %w", externalgateway.ErrInvalidRequest))
	}
	if request.Method != http.MethodPost {
		return nil, rejectRequest(request, fmt.Errorf("external runtime A2A request must use POST: %w", externalgateway.ErrInvalidRequest))
	}
	if request.URL.Path != t.invokePath || request.URL.RawPath != "" || request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.User != nil || request.URL.Fragment != "" || request.URL.RawFragment != "" || request.URL.Opaque != "" {
		return nil, rejectRequest(request, fmt.Errorf("external runtime HTTP request must use its fixed invoke path: %w", externalgateway.ErrInvalidRequest))
	}

	forwarded := request.Clone(&transportContext{Context: request.Context()})
	forwarded.URL = &url.URL{Path: t.invokePath}
	forwarded.Host = ""
	forwarded.RequestURI = ""
	forwarded.Header = protocolHeaders(request.Header)
	forwarded.Trailer = nil
	forwarded.TransferEncoding = nil
	forwarded.GetBody = nil

	return t.broker.RoundTrip(forwarded.Context(), t.slot, forwarded)
}

func rejectRequest(request *http.Request, cause error) error {
	if request.Body == nil {
		return cause
	}
	if err := request.Body.Close(); err != nil {
		return &requestRejectionError{cause: cause, closeErr: err}
	}
	return cause
}

// requestRejectionError keeps both error identities available to recovery code
// without rendering a close error that could contain private URL details.
type requestRejectionError struct {
	cause    error
	closeErr error
}

func (*requestRejectionError) Error() string {
	return "external runtime HTTP request was rejected and its body could not be closed"
}

func (e *requestRejectionError) Unwrap() []error {
	return []error{e.cause, e.closeErr}
}

type transportContext struct {
	context.Context
}

func (*transportContext) Value(any) any {
	return nil
}

func protocolHeaders(source http.Header) http.Header {
	forwarded := make(http.Header, 3)
	for _, name := range [...]string{"Accept", "Content-Type", "X-A2A-Extensions"} {
		for _, value := range source.Values(name) {
			forwarded.Add(name, value)
		}
	}
	return forwarded
}
