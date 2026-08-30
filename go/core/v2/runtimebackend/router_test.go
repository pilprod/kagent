package runtimebackend_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2aclient"
	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/v2/runtimebackend"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRouterValidatesRegistrations(t *testing.T) {
	selector := selectorFunc(func(context.Context, *apiv1alpha1.AgentInstance) (runtimebackend.Kind, error) {
		return runtimebackend.KindSubstrate, nil
	})
	validSubstrate := registration(runtimebackend.KindSubstrate, &recordingBackend{})
	validExternal := registration(runtimebackend.KindExternal, &recordingBackend{})
	var typedNilBackend *recordingBackend

	tests := []struct {
		name          string
		selector      runtimebackend.Selector
		registrations []runtimebackend.Registration
		wantErr       string
	}{
		{name: "nil selector", registrations: []runtimebackend.Registration{validSubstrate, validExternal}, wantErr: "selector is nil"},
		{name: "typed nil selector", selector: selectorFunc(nil), registrations: []runtimebackend.Registration{validSubstrate, validExternal}, wantErr: "selector is nil"},
		{name: "missing registrations", selector: selector, wantErr: "at least one runtime backend must be registered"},
		{
			name:     "duplicate kind",
			selector: selector,
			registrations: []runtimebackend.Registration{
				validSubstrate,
				registration(runtimebackend.KindSubstrate, &recordingBackend{}),
				validExternal,
			},
			wantErr: `backend "substrate" is registered more than once`,
		},
		{
			name:     "unsupported kind",
			selector: selector,
			registrations: []runtimebackend.Registration{
				registration(runtimebackend.Kind("unsupported"), &recordingBackend{}),
				validSubstrate,
				validExternal,
			},
			wantErr: `kind "unsupported" is not supported`,
		},
		{
			name:     "nil lifecycle",
			selector: selector,
			registrations: []runtimebackend.Registration{
				{Kind: runtimebackend.KindSubstrate, Backend: runtimebackend.Backend{Connector: &recordingBackend{}}},
				validExternal,
			},
			wantErr: `backend "substrate" lifecycle is nil`,
		},
		{
			name:     "typed nil lifecycle",
			selector: selector,
			registrations: []runtimebackend.Registration{
				{Kind: runtimebackend.KindSubstrate, Backend: runtimebackend.Backend{Lifecycle: typedNilBackend, Connector: &recordingBackend{}}},
				validExternal,
			},
			wantErr: `backend "substrate" lifecycle is nil`,
		},
		{
			name:     "nil connector",
			selector: selector,
			registrations: []runtimebackend.Registration{
				{Kind: runtimebackend.KindSubstrate, Backend: runtimebackend.Backend{Lifecycle: &recordingBackend{}}},
				validExternal,
			},
			wantErr: `backend "substrate" connector is nil`,
		},
		{
			name:     "typed nil connector",
			selector: selector,
			registrations: []runtimebackend.Registration{
				{Kind: runtimebackend.KindSubstrate, Backend: runtimebackend.Backend{Lifecycle: &recordingBackend{}, Connector: typedNilBackend}},
				validExternal,
			},
			wantErr: `backend "substrate" connector is nil`,
		},
		{name: "substrate only", selector: selector, registrations: []runtimebackend.Registration{validSubstrate}},
		{name: "external only", selector: selector, registrations: []runtimebackend.Registration{validExternal}},
		{name: "complete registry", selector: selector, registrations: []runtimebackend.Registration{validSubstrate, validExternal}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router, err := runtimebackend.NewRouter(tt.selector, tt.registrations...)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				assert.Nil(t, router)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, router)
		})
	}
}

func TestRouterRoutesLifecycleAndConnectorTogether(t *testing.T) {
	tests := []struct {
		name string
		kind runtimebackend.Kind
	}{
		{name: "substrate", kind: runtimebackend.KindSubstrate},
		{name: "external", kind: runtimebackend.KindExternal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := &apiv1alpha1.AgentInstance{Id: "instance-" + tt.name}
			selected := &recordingBackend{
				endpoint: runtimebackend.Endpoint{A2AAuthority: tt.name + ".internal"},
				client:   &a2aclient.Client{},
			}
			other := &recordingBackend{}
			substrate, external := selected, other
			if tt.kind == runtimebackend.KindExternal {
				substrate, external = other, selected
			}
			router := newRouter(t, selectorFunc(func(_ context.Context, got *apiv1alpha1.AgentInstance) (runtimebackend.Kind, error) {
				assert.Same(t, instance, got)
				return tt.kind, nil
			}), substrate, external)

			endpoint, err := router.Create(t.Context(), instance)
			require.NoError(t, err)
			assert.Equal(t, selected.endpoint, endpoint)
			client, err := router.Dial(t.Context(), instance)
			require.NoError(t, err)
			assert.Same(t, selected.client, client)
			require.NoError(t, router.Suspend(t.Context(), instance))
			require.NoError(t, router.Resume(t.Context(), instance))
			require.NoError(t, router.Delete(t.Context(), instance))

			assert.EqualValues(t, 1, selected.createCalls.Load())
			assert.EqualValues(t, 1, selected.dialCalls.Load())
			assert.EqualValues(t, 1, selected.suspendCalls.Load())
			assert.EqualValues(t, 1, selected.resumeCalls.Load())
			assert.EqualValues(t, 1, selected.deleteCalls.Load())
			assert.Zero(t, backendCallCount(other))
		})
	}
}

func TestRouterPreservesForkAndQuiesceSemantics(t *testing.T) {
	instance := &apiv1alpha1.AgentInstance{Id: "external-instance"}
	checkpoint := &dbpkg.AgentInstanceCheckpoint{ID: "checkpoint-1", SnapshotUID: "snapshot-uid"}
	snapshot := &dbpkg.AgentInstanceTaskSnapshot{UID: "snapshot-uid", ContentScope: "FULL"}
	external := &recordingBackend{
		endpoint: runtimebackend.Endpoint{A2AAuthority: "external.internal"},
		snapshot: snapshot,
	}
	router := newRouter(t, staticSelector(runtimebackend.KindExternal), &recordingBackend{}, external)

	endpoint, err := router.Fork(t.Context(), instance, checkpoint)
	require.NoError(t, err)
	assert.Equal(t, external.endpoint, endpoint)
	assert.Same(t, instance, external.forkInstance.Load())
	assert.Same(t, checkpoint, external.forkCheckpoint.Load())
	assert.Zero(t, external.createCalls.Load(), "Fork must not degrade to Create")

	gotSnapshot, err := router.Quiesce(t.Context(), instance)
	require.NoError(t, err)
	assert.Same(t, snapshot, gotSnapshot)
	assert.Same(t, instance, external.quiesceInstance.Load())
	assert.Zero(t, external.suspendCalls.Load(), "Quiesce must preserve snapshot semantics")
}

func TestRouterRejectsMissingSelectionWithoutDefault(t *testing.T) {
	instance := &apiv1alpha1.AgentInstance{
		Id:           "instance-1",
		A2AAuthority: "https://user:runtime-secret@runtime.invalid",
	}
	substrate := &recordingBackend{}
	external := &recordingBackend{}
	router, err := runtimebackend.NewRouter(
		staticSelector(runtimebackend.KindExternal),
		registration(runtimebackend.KindSubstrate, substrate),
	)
	require.NoError(t, err)

	endpoint, err := router.Create(t.Context(), instance)
	require.Error(t, err)
	assert.Empty(t, endpoint)
	assert.Contains(t, err.Error(), `backend "external"`)
	assert.NotContains(t, err.Error(), "runtime-secret")
	assert.Zero(t, backendCallCount(substrate))
	assert.Zero(t, backendCallCount(external))

	client, err := router.Dial(t.Context(), instance)
	require.Error(t, err)
	assert.Nil(t, client)
	assert.Zero(t, backendCallCount(substrate))
	assert.Zero(t, backendCallCount(external))
}

func TestRouterPropagatesContextToSelector(t *testing.T) {
	type contextKey struct{}
	wantValue := new(int)
	ctx := context.WithValue(t.Context(), contextKey{}, wantValue)
	selectorCalled := false
	selector := selectorFunc(func(gotCtx context.Context, instance *apiv1alpha1.AgentInstance) (runtimebackend.Kind, error) {
		selectorCalled = true
		assert.Same(t, wantValue, gotCtx.Value(contextKey{}))
		assert.Equal(t, "instance-1", instance.GetId())
		return runtimebackend.KindSubstrate, nil
	})
	router, err := runtimebackend.NewRouter(selector, registration(runtimebackend.KindSubstrate, &recordingBackend{}))
	require.NoError(t, err)

	_, err = router.Create(ctx, &apiv1alpha1.AgentInstance{Id: "instance-1"})
	require.NoError(t, err)
	assert.True(t, selectorCalled)
}

func TestRouterRejectsNilAgentInstance(t *testing.T) {
	router := newRouter(t, staticSelector(runtimebackend.KindSubstrate), &recordingBackend{}, &recordingBackend{})

	endpoint, err := router.Create(t.Context(), nil)
	require.ErrorContains(t, err, "requires an AgentInstance")
	assert.Empty(t, endpoint)
}

func TestRouterErrorsDoNotLeakRuntimeCredentials(t *testing.T) {
	const credential = "private-runtime-credential"
	cause := fmt.Errorf("connect to https://agent:%s@runtime.invalid", credential)
	partialEndpoint := runtimebackend.Endpoint{A2AAuthority: "agent:" + credential + "@runtime.invalid"}
	partialSnapshot := &dbpkg.AgentInstanceTaskSnapshot{UID: credential}
	partialClient := &a2aclient.Client{}
	failed := &recordingBackend{
		endpoint:   partialEndpoint,
		snapshot:   partialSnapshot,
		client:     partialClient,
		createErr:  cause,
		forkErr:    cause,
		quiesceErr: cause,
		suspendErr: cause,
		resumeErr:  cause,
		deleteErr:  cause,
		dialErr:    cause,
	}
	instance := &apiv1alpha1.AgentInstance{Id: "instance-1", A2AAuthority: partialEndpoint.A2AAuthority}
	router := newRouter(t, staticSelector(runtimebackend.KindExternal), &recordingBackend{}, failed)

	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "create",
			run: func() error {
				endpoint, err := router.Create(t.Context(), instance)
				assert.Empty(t, endpoint)
				return err
			},
		},
		{
			name: "fork",
			run: func() error {
				endpoint, err := router.Fork(t.Context(), instance, &dbpkg.AgentInstanceCheckpoint{})
				assert.Empty(t, endpoint)
				return err
			},
		},
		{
			name: "quiesce",
			run: func() error {
				snapshot, err := router.Quiesce(t.Context(), instance)
				assert.Nil(t, snapshot)
				return err
			},
		},
		{name: "suspend", run: func() error { return router.Suspend(t.Context(), instance) }},
		{name: "resume", run: func() error { return router.Resume(t.Context(), instance) }},
		{name: "delete", run: func() error { return router.Delete(t.Context(), instance) }},
		{
			name: "dial",
			run: func() error {
				client, err := router.Dial(t.Context(), instance)
				assert.Nil(t, client)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run()
			require.Error(t, err)
			assert.ErrorIs(t, err, cause)
			assert.NotContains(t, err.Error(), credential)
			assert.NotContains(t, err.Error(), "runtime.invalid")
		})
	}

	selectorCause := fmt.Errorf("selector inspected %s", credential)
	selectorRouter := newRouter(t, selectorFunc(func(context.Context, *apiv1alpha1.AgentInstance) (runtimebackend.Kind, error) {
		return "", selectorCause
	}), &recordingBackend{}, &recordingBackend{})
	_, err := selectorRouter.Create(t.Context(), instance)
	require.Error(t, err)
	assert.ErrorIs(t, err, selectorCause)
	assert.NotContains(t, err.Error(), credential)
}

func TestRouterSupportsConcurrentRouting(t *testing.T) {
	const operationsPerBackend = int64(32)
	substrate := &recordingBackend{endpoint: runtimebackend.Endpoint{A2AAuthority: "substrate.internal"}, client: &a2aclient.Client{}}
	external := &recordingBackend{endpoint: runtimebackend.Endpoint{A2AAuthority: "external.internal"}, client: &a2aclient.Client{}}
	router := newRouter(t, selectorFunc(func(_ context.Context, instance *apiv1alpha1.AgentInstance) (runtimebackend.Kind, error) {
		switch instance.GetId() {
		case "substrate-instance":
			return runtimebackend.KindSubstrate, nil
		case "external-instance":
			return runtimebackend.KindExternal, nil
		default:
			return "", fmt.Errorf("no selection")
		}
	}), substrate, external)

	errCh := make(chan error, 2*operationsPerBackend*7)
	var workers sync.WaitGroup
	for i := int64(0); i < 2*operationsPerBackend; i++ {
		kind := runtimebackend.KindSubstrate
		if i%2 == 1 {
			kind = runtimebackend.KindExternal
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			instance := &apiv1alpha1.AgentInstance{Id: string(kind) + "-instance"}
			checkpoint := &dbpkg.AgentInstanceCheckpoint{ID: "checkpoint-1"}
			_, err := router.Create(t.Context(), instance)
			recordError(errCh, err)
			_, err = router.Fork(t.Context(), instance, checkpoint)
			recordError(errCh, err)
			_, err = router.Quiesce(t.Context(), instance)
			recordError(errCh, err)
			recordError(errCh, router.Suspend(t.Context(), instance))
			recordError(errCh, router.Resume(t.Context(), instance))
			recordError(errCh, router.Delete(t.Context(), instance))
			_, err = router.Dial(t.Context(), instance)
			recordError(errCh, err)
		}()
	}
	workers.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	for _, backend := range []*recordingBackend{substrate, external} {
		assert.Equal(t, operationsPerBackend, backend.createCalls.Load())
		assert.Equal(t, operationsPerBackend, backend.forkCalls.Load())
		assert.Equal(t, operationsPerBackend, backend.quiesceCalls.Load())
		assert.Equal(t, operationsPerBackend, backend.suspendCalls.Load())
		assert.Equal(t, operationsPerBackend, backend.resumeCalls.Load())
		assert.Equal(t, operationsPerBackend, backend.deleteCalls.Load())
		assert.Equal(t, operationsPerBackend, backend.dialCalls.Load())
	}
}

type selectorFunc func(context.Context, *apiv1alpha1.AgentInstance) (runtimebackend.Kind, error)

func (f selectorFunc) Select(ctx context.Context, instance *apiv1alpha1.AgentInstance) (runtimebackend.Kind, error) {
	return f(ctx, instance)
}

func staticSelector(kind runtimebackend.Kind) runtimebackend.Selector {
	return selectorFunc(func(context.Context, *apiv1alpha1.AgentInstance) (runtimebackend.Kind, error) {
		return kind, nil
	})
}

type recordingBackend struct {
	endpoint runtimebackend.Endpoint
	snapshot *dbpkg.AgentInstanceTaskSnapshot
	client   *a2aclient.Client

	createErr  error
	forkErr    error
	quiesceErr error
	suspendErr error
	resumeErr  error
	deleteErr  error
	dialErr    error

	createCalls     atomic.Int64
	forkCalls       atomic.Int64
	quiesceCalls    atomic.Int64
	suspendCalls    atomic.Int64
	resumeCalls     atomic.Int64
	deleteCalls     atomic.Int64
	dialCalls       atomic.Int64
	forkInstance    atomic.Pointer[apiv1alpha1.AgentInstance]
	forkCheckpoint  atomic.Pointer[dbpkg.AgentInstanceCheckpoint]
	quiesceInstance atomic.Pointer[apiv1alpha1.AgentInstance]
}

var (
	_ runtimebackend.Lifecycle = (*recordingBackend)(nil)
	_ runtimebackend.Connector = (*recordingBackend)(nil)
)

func (r *recordingBackend) Create(context.Context, *apiv1alpha1.AgentInstance) (runtimebackend.Endpoint, error) {
	r.createCalls.Add(1)
	return r.endpoint, r.createErr
}

func (r *recordingBackend) Fork(_ context.Context, instance *apiv1alpha1.AgentInstance, checkpoint *dbpkg.AgentInstanceCheckpoint) (runtimebackend.Endpoint, error) {
	r.forkCalls.Add(1)
	r.forkInstance.Store(instance)
	r.forkCheckpoint.Store(checkpoint)
	return r.endpoint, r.forkErr
}

func (r *recordingBackend) Quiesce(_ context.Context, instance *apiv1alpha1.AgentInstance) (*dbpkg.AgentInstanceTaskSnapshot, error) {
	r.quiesceCalls.Add(1)
	r.quiesceInstance.Store(instance)
	return r.snapshot, r.quiesceErr
}

func (r *recordingBackend) Suspend(context.Context, *apiv1alpha1.AgentInstance) error {
	r.suspendCalls.Add(1)
	return r.suspendErr
}

func (r *recordingBackend) Resume(context.Context, *apiv1alpha1.AgentInstance) error {
	r.resumeCalls.Add(1)
	return r.resumeErr
}

func (r *recordingBackend) Delete(context.Context, *apiv1alpha1.AgentInstance) error {
	r.deleteCalls.Add(1)
	return r.deleteErr
}

func (r *recordingBackend) Dial(context.Context, *apiv1alpha1.AgentInstance) (*a2aclient.Client, error) {
	r.dialCalls.Add(1)
	return r.client, r.dialErr
}

func registration(kind runtimebackend.Kind, implementation *recordingBackend) runtimebackend.Registration {
	return runtimebackend.Registration{
		Kind: kind,
		Backend: runtimebackend.Backend{
			Lifecycle: implementation,
			Connector: implementation,
		},
	}
}

func newRouter(t *testing.T, selector runtimebackend.Selector, substrate, external *recordingBackend) *runtimebackend.Router {
	t.Helper()
	router, err := runtimebackend.NewRouter(
		selector,
		registration(runtimebackend.KindSubstrate, substrate),
		registration(runtimebackend.KindExternal, external),
	)
	require.NoError(t, err)
	return router
}

func backendCallCount(backend *recordingBackend) int64 {
	return backend.createCalls.Load() + backend.forkCalls.Load() + backend.quiesceCalls.Load() +
		backend.suspendCalls.Load() + backend.resumeCalls.Load() + backend.deleteCalls.Load() + backend.dialCalls.Load()
}

func recordError(errCh chan<- error, err error) {
	if err != nil {
		errCh <- err
	}
}
