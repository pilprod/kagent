package substrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aext"
	a2agrpc "github.com/a2aproject/a2a-go/v2/a2agrpc/v1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/uuid"
	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	externalRuntimeTarget    = "passthrough:///external-slot"
	maximumActorIngressBytes = 64 << 10
)

var errActorIngressReset = errors.New("Actor ingress was reset")

type actorIngressStream interface {
	Context() context.Context
	Send(*ateapipb.ActorIngressFrame) error
	Recv() (*ateapipb.ActorIngressFrame, error)
	CloseSend() error
}

type actorIngressAPI interface {
	GetActor(context.Context, *ateapipb.GetActorRequest) (*ateapipb.Actor, error)
	OpenActorIngress(context.Context) (actorIngressStream, error)
}

type substrateActorIngressAPI struct {
	control ateapipb.ControlClient
}

func (api substrateActorIngressAPI) GetActor(ctx context.Context, request *ateapipb.GetActorRequest) (*ateapipb.Actor, error) {
	return api.control.GetActor(ctx, request)
}

func (api substrateActorIngressAPI) OpenActorIngress(ctx context.Context) (actorIngressStream, error) {
	return api.control.OpenActorIngress(ctx)
}

func (c *Connector) dialExternalSlot(
	ctx context.Context,
	instance *apiv1alpha1.AgentInstance,
	revision *dbpkg.RuntimeRevision,
) (*a2aclient.Client, error) {
	if c.ingress == nil {
		return nil, fmt.Errorf("ExternalSlot Actor ingress is not configured")
	}
	opener := &singleUseActorIngressDialer{
		open: func(dialCtx context.Context) (net.Conn, error) {
			return openActorIngress(dialCtx, c.ingress, instance, revision)
		},
	}
	return newExternalRuntimeClient(ctx, instance.GetA2AAuthority(), opener)
}

func newExternalRuntimeClient(
	ctx context.Context,
	authority string,
	opener *singleUseActorIngressDialer,
) (*a2aclient.Client, error) {
	// The outer Control stream already supplies authenticated mTLS transport.
	// The nested A2A gRPC connection therefore uses plaintext only inside that
	// protected stream. Do not forward the caller's cluster credential to a
	// runtime on a user-owned machine; the gateway has already authorized the
	// operation and the exact Actor/Worker route is authenticated by Substrate.
	return a2aclient.NewFromEndpoints(ctx, []*a2atype.AgentInterface{{
		URL:             externalRuntimeTarget,
		ProtocolBinding: a2atype.TransportProtocolGRPC,
		ProtocolVersion: a2atype.Version,
	}},
		a2agrpc.WithGRPCTransport(
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithAuthority(authority),
			grpc.WithContextDialer(opener.DialContext),
		),
		a2aclient.WithCallInterceptors(a2aext.NewClientPropagator(nil)),
	)
}

type singleUseActorIngressDialer struct {
	used atomic.Bool
	open func(context.Context) (net.Conn, error)
}

func (d *singleUseActorIngressDialer) DialContext(ctx context.Context, _ string) (net.Conn, error) {
	if d == nil || d.open == nil {
		return nil, fmt.Errorf("Actor ingress dialer is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !d.used.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("Actor ingress does not reconnect across a fenced assignment")
	}
	return d.open(ctx)
}

func openActorIngress(
	ctx context.Context,
	api actorIngressAPI,
	instance *apiv1alpha1.AgentInstance,
	revision *dbpkg.RuntimeRevision,
) (net.Conn, error) {
	if ctx == nil || api == nil || instance == nil || revision == nil {
		return nil, fmt.Errorf("Actor ingress identity is incomplete")
	}
	actorRef := &ateapipb.ObjectRef{
		Atespace: instance.GetNamespace(),
		Name:     actorName(instance.GetId()),
	}
	actor, err := api.GetActor(ctx, &ateapipb.GetActorRequest{Actor: actorRef})
	if err != nil {
		return nil, fmt.Errorf("resolve ExternalSlot Actor: %w", err)
	}
	metadata := actor.GetMetadata()
	uid, err := uuid.Parse(metadata.GetUid())
	if err != nil || uid.String() != metadata.GetUid() || metadata.GetAtespace() != actorRef.GetAtespace() || metadata.GetName() != actorRef.GetName() {
		return nil, fmt.Errorf("ExternalSlot Actor returned an invalid identity")
	}
	if actor.GetActorTemplateNamespace() != revision.ActorTemplateNamespace || actor.GetActorTemplateName() != revision.ActorTemplateName {
		return nil, fmt.Errorf("ExternalSlot Actor uses an unexpected runtime revision")
	}
	if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING {
		return nil, fmt.Errorf("ExternalSlot Actor is not running")
	}

	// gRPC owns the context passed to a custom ContextDialer and cancels it as
	// soon as DialContext returns. Actor ingress is the connection returned by
	// that dialer, so binding the long-lived Control stream directly to ctx
	// tears the stream down immediately after a successful dial. Preserve the
	// dial context values, mirror cancellation only while the ingress handshake
	// is in progress, and let actorIngressConn.Close own cancellation after the
	// connection has been handed to gRPC.
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopDialCancellation := context.AfterFunc(ctx, cancel)
	stream, err := api.OpenActorIngress(streamCtx)
	if err != nil {
		stopDialCancellation()
		cancel()
		return nil, fmt.Errorf("open ExternalSlot Actor ingress: %w", err)
	}
	fail := func(err error) (net.Conn, error) {
		stopDialCancellation()
		_ = stream.CloseSend()
		cancel()
		return nil, err
	}
	if err := stream.Send(&ateapipb.ActorIngressFrame{Frame: &ateapipb.ActorIngressFrame_Open{Open: &ateapipb.ActorIngressOpen{
		Actor: actorRef, ActorUid: metadata.GetUid(),
	}}}); err != nil {
		return fail(fmt.Errorf("send ExternalSlot Actor ingress identity: %w", err))
	}
	frame, err := stream.Recv()
	if err != nil {
		return fail(fmt.Errorf("confirm ExternalSlot Actor ingress: %w", err))
	}
	if frame == nil || frame.GetOpened() == nil {
		return fail(fmt.Errorf("ExternalSlot Actor ingress returned an invalid acknowledgement"))
	}
	if !stopDialCancellation() {
		return fail(fmt.Errorf("confirm ExternalSlot Actor ingress: %w", context.Cause(ctx)))
	}
	return &actorIngressConn{stream: stream, cancel: cancel}, nil
}

type actorIngressConn struct {
	stream actorIngressStream
	cancel context.CancelFunc

	readMu    sync.Mutex
	pending   []byte
	readEOF   bool
	writeMu   sync.Mutex
	closeOnce sync.Once
	closed    atomic.Bool
}

func (c *actorIngressConn) Read(destination []byte) (int, error) {
	if len(destination) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for {
		if len(c.pending) != 0 {
			count := copy(destination, c.pending)
			c.pending = c.pending[count:]
			return count, nil
		}
		if c.readEOF {
			return 0, io.EOF
		}
		if c.closed.Load() {
			return 0, net.ErrClosed
		}
		frame, err := c.stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, io.ErrUnexpectedEOF
			}
			return 0, fmt.Errorf("receive Actor ingress frame: %w", err)
		}
		if frame == nil {
			return 0, fmt.Errorf("Actor ingress returned an empty frame")
		}
		switch member := frame.GetFrame().(type) {
		case *ateapipb.ActorIngressFrame_Data:
			if len(member.Data) == 0 || len(member.Data) > maximumActorIngressBytes {
				return 0, fmt.Errorf("Actor ingress returned an invalid data frame")
			}
			c.pending = slices.Clone(member.Data)
		case *ateapipb.ActorIngressFrame_HalfClose:
			if member.HalfClose == nil {
				return 0, fmt.Errorf("Actor ingress returned an invalid half-close")
			}
			c.readEOF = true
		case *ateapipb.ActorIngressFrame_Reset_:
			if member.Reset_ == nil {
				return 0, fmt.Errorf("Actor ingress returned an invalid reset")
			}
			return 0, errActorIngressReset
		default:
			return 0, fmt.Errorf("Actor ingress returned an out-of-order frame")
		}
	}
}

func (c *actorIngressConn) Write(source []byte) (int, error) {
	if len(source) == 0 {
		return 0, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	written := 0
	for len(source) != 0 {
		count := min(len(source), maximumActorIngressBytes)
		if err := c.stream.Send(&ateapipb.ActorIngressFrame{Frame: &ateapipb.ActorIngressFrame_Data{Data: slices.Clone(source[:count])}}); err != nil {
			return written, fmt.Errorf("send Actor ingress frame: %w", err)
		}
		written += count
		source = source[count:]
	}
	return written, nil
}

func (c *actorIngressConn) Close() error {
	var closeErr error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		c.writeMu.Lock()
		if err := c.stream.Send(&ateapipb.ActorIngressFrame{Frame: &ateapipb.ActorIngressFrame_Reset_{Reset_: &ateapipb.ActorIngressReset{}}}); err != nil {
			closeErr = err
		}
		if err := c.stream.CloseSend(); closeErr == nil && err != nil {
			closeErr = err
		}
		c.writeMu.Unlock()
		c.cancel()
	})
	return closeErr
}

func (*actorIngressConn) LocalAddr() net.Addr              { return actorIngressAddr("kagent") }
func (*actorIngressConn) RemoteAddr() net.Addr             { return actorIngressAddr("external-runtime") }
func (*actorIngressConn) SetDeadline(time.Time) error      { return nil }
func (*actorIngressConn) SetReadDeadline(time.Time) error  { return nil }
func (*actorIngressConn) SetWriteDeadline(time.Time) error { return nil }

type actorIngressAddr string

func (actorIngressAddr) Network() string        { return "substrate-actor-ingress" }
func (address actorIngressAddr) String() string { return string(address) }

var _ net.Conn = (*actorIngressConn)(nil)
