package substrate

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aext"
	a2agrpc "github.com/a2aproject/a2a-go/v2/a2agrpc/v1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/kagent-dev/kagent/go/core/v2/runtimebackend"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Connector connects public gateway calls to the root Actor through
// Substrate's shared Atenet router.
type Connector struct {
	target        string
	transport     credentials.TransportCredentials
	authenticator auth.AuthProvider
	revisions     RuntimeRevisionStore
	ingress       actorIngressAPI
}

// RuntimeRevisionStore resolves the immutable execution placement selected by
// the compiler. It is deliberately narrower than the gateway store so runtime
// routing cannot mutate control-plane state.
type RuntimeRevisionStore interface {
	GetRuntimeRevision(context.Context, string) (*dbpkg.RuntimeRevision, error)
}

var _ runtimebackend.Connector = (*Connector)(nil)

func NewConnector(routerURL string, authenticator auth.AuthProvider) (*Connector, error) {
	router, err := url.Parse(routerURL)
	if err != nil {
		return nil, fmt.Errorf("parse Atenet router URL %q: %w", routerURL, err)
	}
	if router.Host == "" {
		return nil, fmt.Errorf("atenet router URL %q must include a host", routerURL)
	}
	if authenticator == nil {
		return nil, fmt.Errorf("atenet runtime authentication is not configured")
	}
	var transport credentials.TransportCredentials
	switch router.Scheme {
	case "http":
		transport = insecure.NewCredentials()
	case "https":
		transport = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: router.Hostname()})
	default:
		return nil, fmt.Errorf("atenet router URL %q must use http or https", routerURL)
	}
	return &Connector{target: router.Host, transport: transport, authenticator: authenticator}, nil
}

// NewProviderAwareConnector adds the authenticated Substrate Control path used
// by ExternalSlot revisions. KubernetesPod revisions continue to use Atenet.
// Both dependencies are mandatory so production wiring cannot silently route
// an external runtime through cluster DNS.
func NewProviderAwareConnector(
	routerURL string,
	authenticator auth.AuthProvider,
	revisions RuntimeRevisionStore,
	control ateapipb.ControlClient,
) (*Connector, error) {
	if revisions == nil {
		return nil, fmt.Errorf("runtime revision store is required")
	}
	if control == nil {
		return nil, fmt.Errorf("Substrate Actor ingress client is required")
	}
	connector, err := NewConnector(routerURL, authenticator)
	if err != nil {
		return nil, err
	}
	connector.revisions = revisions
	connector.ingress = substrateActorIngressAPI{control: control}
	return connector, nil
}

func (c *Connector) Dial(ctx context.Context, instance *apiv1alpha1.AgentInstance) (*a2aclient.Client, error) {
	if c == nil || instance == nil {
		return nil, fmt.Errorf("runtime connector and AgentInstance are required")
	}
	if instance.GetA2AAuthority() == "" {
		return nil, fmt.Errorf("runtime authority is empty")
	}
	placement, revision, err := c.runtimePlacement(ctx, instance)
	if err != nil {
		return nil, err
	}
	if placement == dbpkg.RuntimeRevisionPlacementExternalSlot {
		return c.dialExternalSlot(ctx, instance, revision)
	}
	return c.dialAtenet(ctx, instance)
}

func (c *Connector) runtimePlacement(ctx context.Context, instance *apiv1alpha1.AgentInstance) (dbpkg.RuntimeRevisionPlacement, *dbpkg.RuntimeRevision, error) {
	// The legacy constructor remains Kubernetes-only for focused callers and
	// tests. Production wiring uses the provider-aware constructor above.
	if c.revisions == nil {
		return dbpkg.RuntimeRevisionPlacementKubernetesPod, nil, nil
	}
	if instance.GetPreparedRevision() == "" {
		return "", nil, fmt.Errorf("prepared runtime revision is empty")
	}
	revision, err := c.revisions.GetRuntimeRevision(ctx, instance.GetPreparedRevision())
	if err != nil {
		return "", nil, fmt.Errorf("load prepared runtime revision: %w", err)
	}
	if revision == nil || revision.Revision != instance.GetPreparedRevision() {
		return "", nil, fmt.Errorf("prepared runtime revision identity is invalid")
	}
	placement, err := dbpkg.NormalizeRuntimeRevisionPlacement(revision.Placement)
	if err != nil {
		return "", nil, err
	}
	return placement, revision, nil
}

func (c *Connector) dialAtenet(ctx context.Context, instance *apiv1alpha1.AgentInstance) (*a2aclient.Client, error) {
	// ponytail: scope one connection to one public RPC until gateway traffic
	// justifies a lifecycle-aware per-instance connection pool.
	return a2aclient.NewFromEndpoints(ctx, []*a2atype.AgentInterface{{
		URL:             c.target,
		ProtocolBinding: a2atype.TransportProtocolGRPC,
		ProtocolVersion: a2atype.Version,
	}},
		a2agrpc.WithGRPCTransport(
			grpc.WithTransportCredentials(c.transport),
			grpc.WithAuthority(instance.GetA2AAuthority()),
		),
		a2aclient.WithCallInterceptors(
			a2aext.NewClientPropagator(nil),
			&upstreamAuthInterceptor{authenticator: c.authenticator, instance: instance},
		),
	)
}

// upstreamAuthInterceptor mirrors the current gateway's per-request auth
// forwarding. ServiceParams make the resulting headers transport-neutral: the
// A2A gRPC transport carries them as metadata to the private runtime.
type upstreamAuthInterceptor struct {
	a2aclient.PassthroughInterceptor
	authenticator auth.AuthProvider
	instance      *apiv1alpha1.AgentInstance
}

func (u *upstreamAuthInterceptor) Before(ctx context.Context, req *a2aclient.Request) (context.Context, any, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+req.BaseURL, nil)
	if err != nil {
		return ctx, nil, err
	}
	if session, ok := auth.AuthSessionFrom(ctx); ok {
		principal := auth.Principal{Agent: auth.Agent{ID: u.instance.GetNamespace() + "/" + u.instance.GetId()}}
		if err := u.authenticator.UpstreamAuth(httpRequest, session, principal); err != nil {
			return ctx, nil, err
		}
	}
	propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(httpRequest.Header))
	for key, values := range httpRequest.Header {
		for _, value := range values {
			req.ServiceParams.Append(key, value)
		}
	}
	return ctx, nil, nil
}
