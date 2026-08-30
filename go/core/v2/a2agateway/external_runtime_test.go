package a2agateway

import (
	"context"
	"errors"
	"io"
	"iter"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	a2agrpc "github.com/a2aproject/a2a-go/v2/a2agrpc/v1"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

const externalRuntimeTestActorUID = "00000000-0000-4000-8000-000000000401"

type externalRuntimeTestAPI struct {
	actor  *ateapipb.Actor
	stream *externalRuntimeTestStream

	mu      sync.Mutex
	request *ateapipb.GetActorRequest
}

func (api *externalRuntimeTestAPI) GetActor(_ context.Context, request *ateapipb.GetActorRequest) (*ateapipb.Actor, error) {
	api.mu.Lock()
	api.request = proto.Clone(request).(*ateapipb.GetActorRequest)
	api.mu.Unlock()
	return proto.Clone(api.actor).(*ateapipb.Actor), nil
}

func (api *externalRuntimeTestAPI) OpenActorIngress(ctx context.Context) (actorIngressStream, error) {
	api.stream.ctx = ctx
	return api.stream, nil
}

type externalRuntimeTestStream struct {
	ctx    context.Context
	recv   chan *ateapipb.ActorIngressFrame
	closed chan struct{}

	mu   sync.Mutex
	sent []*ateapipb.ActorIngressFrame
	once sync.Once
}

func newExternalRuntimeTestStream(frames ...*ateapipb.ActorIngressFrame) *externalRuntimeTestStream {
	recv := make(chan *ateapipb.ActorIngressFrame, len(frames))
	for _, frame := range frames {
		recv <- proto.Clone(frame).(*ateapipb.ActorIngressFrame)
	}
	return &externalRuntimeTestStream{ctx: context.Background(), recv: recv, closed: make(chan struct{})}
}

func (stream *externalRuntimeTestStream) Context() context.Context { return stream.ctx }

func (stream *externalRuntimeTestStream) Send(frame *ateapipb.ActorIngressFrame) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	stream.sent = append(stream.sent, proto.Clone(frame).(*ateapipb.ActorIngressFrame))
	return nil
}

func (stream *externalRuntimeTestStream) Recv() (*ateapipb.ActorIngressFrame, error) {
	select {
	case frame, ok := <-stream.recv:
		if !ok {
			return nil, io.EOF
		}
		return proto.Clone(frame).(*ateapipb.ActorIngressFrame), nil
	case <-stream.closed:
		return nil, io.EOF
	case <-stream.ctx.Done():
		return nil, context.Cause(stream.ctx)
	}
}

func (stream *externalRuntimeTestStream) CloseSend() error {
	stream.once.Do(func() { close(stream.closed) })
	return nil
}

func (stream *externalRuntimeTestStream) sentFrames() []*ateapipb.ActorIngressFrame {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	result := make([]*ateapipb.ActorIngressFrame, 0, len(stream.sent))
	for _, frame := range stream.sent {
		result = append(result, proto.Clone(frame).(*ateapipb.ActorIngressFrame))
	}
	return result
}

func TestOpenActorIngressFencesIdentityAndBridgesBytes(t *testing.T) {
	instance, revision, actor := externalRuntimeTestIdentity()
	stream := newExternalRuntimeTestStream(
		&ateapipb.ActorIngressFrame{Frame: &ateapipb.ActorIngressFrame_Opened{Opened: &ateapipb.ActorIngressOpened{}}},
		&ateapipb.ActorIngressFrame{Frame: &ateapipb.ActorIngressFrame_Data{Data: []byte("hello")}},
		&ateapipb.ActorIngressFrame{Frame: &ateapipb.ActorIngressFrame_HalfClose{HalfClose: &ateapipb.ActorIngressHalfClose{}}},
	)
	api := &externalRuntimeTestAPI{actor: actor, stream: stream}
	connection, err := openActorIngress(t.Context(), api, instance, revision)
	if err != nil {
		t.Fatal(err)
	}

	buffer := make([]byte, 3)
	first, err := connection.Read(buffer)
	if err != nil || first != 3 || string(buffer) != "hel" {
		t.Fatalf("first Read() = %q, %d, %v", buffer, first, err)
	}
	second, err := connection.Read(buffer)
	if err != nil || second != 2 || string(buffer[:second]) != "lo" {
		t.Fatalf("second Read() = %q, %d, %v", buffer[:second], second, err)
	}
	if count, err := connection.Read(buffer); count != 0 || err != io.EOF {
		t.Fatalf("terminal Read() = %d, %v, want EOF", count, err)
	}

	payload := make([]byte, maximumActorIngressBytes+1)
	for index := range payload {
		payload[index] = byte(index)
	}
	if count, err := connection.Write(payload); err != nil || count != len(payload) {
		t.Fatalf("Write() = %d, %v", count, err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}

	frames := stream.sentFrames()
	if len(frames) != 4 {
		t.Fatalf("sent frame count = %d, want open, two data, reset", len(frames))
	}
	open := frames[0].GetOpen()
	if open.GetActor().GetAtespace() != instance.GetNamespace() || open.GetActor().GetName() != externalActorName(instance.GetId()) || open.GetActorUid() != externalRuntimeTestActorUID {
		t.Fatalf("open identity = %#v", open)
	}
	if got := append(slices.Clone(frames[1].GetData()), frames[2].GetData()...); !slices.Equal(got, payload) {
		t.Fatal("data frames did not preserve the payload")
	}
	if frames[3].GetReset_() == nil {
		t.Fatalf("last frame = %#v, want reset", frames[3])
	}
	if request := api.request; request.GetActor().GetAtespace() != instance.GetNamespace() || request.GetActor().GetName() != externalActorName(instance.GetId()) {
		t.Fatalf("GetActor request = %#v", request)
	}
}

func TestOpenActorIngressSurvivesDialContextCancellationAfterHandshake(t *testing.T) {
	instance, revision, actor := externalRuntimeTestIdentity()
	stream := newExternalRuntimeTestStream(openedActorIngressFrame())
	api := &externalRuntimeTestAPI{actor: actor, stream: stream}
	dialCtx, cancelDial := context.WithCancel(t.Context())
	connection, err := openActorIngress(dialCtx, api, instance, revision)
	if err != nil {
		cancelDial()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	// grpc cancels a custom dialer's context once it has accepted the returned
	// net.Conn. The established Actor ingress stream must remain usable until
	// that net.Conn itself is closed.
	cancelDial()
	stream.recv <- &ateapipb.ActorIngressFrame{Frame: &ateapipb.ActorIngressFrame_Data{Data: []byte("still-open")}}
	buffer := make([]byte, len("still-open"))
	count, err := connection.Read(buffer)
	if err != nil || count != len(buffer) || string(buffer) != "still-open" {
		t.Fatalf("Read() after dial context cancellation = %q, %d, %v", buffer[:count], count, err)
	}
}

func TestOpenActorIngressHonorsDialContextCancellationBeforeHandshake(t *testing.T) {
	instance, revision, actor := externalRuntimeTestIdentity()
	stream := newExternalRuntimeTestStream()
	api := &externalRuntimeTestAPI{actor: actor, stream: stream}
	dialCtx, cancelDial := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		connection, err := openActorIngress(dialCtx, api, instance, revision)
		if connection != nil {
			_ = connection.Close()
		}
		result <- err
	}()

	cancelDial()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("openActorIngress() after pre-handshake cancellation = %v, want context.Canceled", err)
	}
}

func TestActorIngressConnectionCloseCancelsDurableStream(t *testing.T) {
	instance, revision, actor := externalRuntimeTestIdentity()
	stream := newExternalRuntimeTestStream(openedActorIngressFrame())
	api := &externalRuntimeTestAPI{actor: actor, stream: stream}
	connection, err := openActorIngress(t.Context(), api, instance, revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stream.ctx.Done():
	case <-t.Context().Done():
		t.Fatal("Actor ingress stream remained live after connection Close")
	}
}

func TestOpenActorIngressRejectsRevisionAndProtocolMismatch(t *testing.T) {
	instance, revision, actor := externalRuntimeTestIdentity()
	tests := []struct {
		name   string
		mutate func(*dbpkg.RuntimeRevision, *ateapipb.Actor)
		ack    *ateapipb.ActorIngressFrame
	}{
		{name: "template", mutate: func(_ *dbpkg.RuntimeRevision, actor *ateapipb.Actor) { actor.ActorTemplateName = "other" }, ack: openedActorIngressFrame()},
		{name: "actor uid", mutate: func(_ *dbpkg.RuntimeRevision, actor *ateapipb.Actor) { actor.Metadata.Uid = "not-a-uid" }, ack: openedActorIngressFrame()},
		{name: "actor state", mutate: func(_ *dbpkg.RuntimeRevision, actor *ateapipb.Actor) {
			actor.Status.State = ateapipb.ActorState_ACTOR_STATE_SUSPENDED
		}, ack: openedActorIngressFrame()},
		{name: "acknowledgement", mutate: func(*dbpkg.RuntimeRevision, *ateapipb.Actor) {}, ack: &ateapipb.ActorIngressFrame{Frame: &ateapipb.ActorIngressFrame_Data{Data: []byte("early")}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidateRevision := *revision
			candidateActor := proto.Clone(actor).(*ateapipb.Actor)
			test.mutate(&candidateRevision, candidateActor)
			api := &externalRuntimeTestAPI{actor: candidateActor, stream: newExternalRuntimeTestStream(test.ack)}
			connection, err := openActorIngress(t.Context(), api, instance, &candidateRevision)
			if err == nil || connection != nil {
				t.Fatalf("openActorIngress() = %#v, %v, want rejection", connection, err)
			}
		})
	}
}

func TestActorIngressConnectionRejectsResetAndTruncatedStream(t *testing.T) {
	for _, test := range []struct {
		name  string
		frame *ateapipb.ActorIngressFrame
		want  error
	}{
		{name: "reset", frame: &ateapipb.ActorIngressFrame{Frame: &ateapipb.ActorIngressFrame_Reset_{Reset_: &ateapipb.ActorIngressReset{}}}, want: errActorIngressReset},
		{name: "truncated", frame: nil, want: io.ErrUnexpectedEOF},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stream *externalRuntimeTestStream
			if test.frame != nil {
				stream = newExternalRuntimeTestStream(test.frame)
			} else {
				stream = newExternalRuntimeTestStream()
				close(stream.recv)
			}
			connection := &actorIngressConn{stream: stream, cancel: func() {}}
			_, err := connection.Read(make([]byte, 1))
			if !errors.Is(err, test.want) {
				t.Fatalf("Read() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestRuntimePlacementRequiresExactPersistedRevision(t *testing.T) {
	instance, revision, _ := externalRuntimeTestIdentity()
	store := &gatewayTestStore{revision: revision}
	dialer := &RuntimeDialer{revisions: store}
	placement, got, err := dialer.runtimePlacement(t.Context(), instance)
	if err != nil || placement != dbpkg.RuntimeRevisionPlacementExternalSlot || got != revision {
		t.Fatalf("runtimePlacement() = %q, %#v, %v", placement, got, err)
	}
	revision.Revision = "changed"
	if _, _, err := dialer.runtimePlacement(t.Context(), instance); err == nil {
		t.Fatal("runtimePlacement() accepted a mismatched revision identity")
	}
}

func TestExternalRuntimeClientUsesPassThroughDialerAfterFactoryContextEnds(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	a2agrpc.NewHandler(a2asrv.NewHandler(externalRuntimeEchoExecutor{})).RegisterWith(server)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	var opened atomic.Bool
	opener := &singleUseActorIngressDialer{open: func(ctx context.Context) (net.Conn, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		opened.Store(true)
		return listener.DialContext(ctx)
	}}
	factoryCtx, cancelFactory := context.WithCancel(t.Context())
	client, err := newExternalRuntimeClient(factoryCtx, "runtime.internal", opener)
	if err != nil {
		cancelFactory()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Destroy() })
	cancelFactory()

	result, err := client.SendMessage(t.Context(), &a2atype.SendMessageRequest{
		Message: a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("hello")),
	})
	if err != nil {
		t.Fatalf("SendMessage() through Actor ingress dialer: %v", err)
	}
	if result == nil || !opened.Load() {
		t.Fatalf("SendMessage() result = %#v, custom dialer called = %t", result, opened.Load())
	}
}

type externalRuntimeEchoExecutor struct{}

func (externalRuntimeEchoExecutor) Execute(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2atype.Event, error] {
	return func(yield func(a2atype.Event, error) bool) {
		yield(a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart("ok")), nil)
	}
}

func (externalRuntimeEchoExecutor) Cancel(_ context.Context, execution *a2asrv.ExecutorContext) iter.Seq2[a2atype.Event, error] {
	return func(yield func(a2atype.Event, error) bool) {
		yield(a2atype.NewStatusUpdateEvent(execution, a2atype.TaskStateCanceled, nil), nil)
	}
}

func openedActorIngressFrame() *ateapipb.ActorIngressFrame {
	return &ateapipb.ActorIngressFrame{Frame: &ateapipb.ActorIngressFrame_Opened{Opened: &ateapipb.ActorIngressOpened{}}}
}

func externalRuntimeTestIdentity() (*apiv1alpha1.AgentInstance, *dbpkg.RuntimeRevision, *ateapipb.Actor) {
	instance := &apiv1alpha1.AgentInstance{
		Id: gatewayTestID, Namespace: "team-a", PreparedRevision: "revision-external", A2AAuthority: "runtime.internal",
	}
	revision := &dbpkg.RuntimeRevision{
		Revision: "revision-external", Placement: dbpkg.RuntimeRevisionPlacementExternalSlot,
		ActorTemplateNamespace: "team-a", ActorTemplateName: "template-external",
	}
	actor := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace: instance.GetNamespace(), Name: externalActorName(instance.GetId()), Uid: externalRuntimeTestActorUID,
		},
		ActorTemplateNamespace: revision.ActorTemplateNamespace,
		ActorTemplateName:      revision.ActorTemplateName,
		Status:                 &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}
	return instance, revision, actor
}

var _ net.Conn = (*actorIngressConn)(nil)
