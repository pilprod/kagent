package main

import (
	"context"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2aclient"
	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/v2/runtimebackend"
)

type controllerRevisionStoreFunc func(context.Context, string) (*dbpkg.RuntimeRevision, error)

func (f controllerRevisionStoreFunc) GetRuntimeRevision(ctx context.Context, revision string) (*dbpkg.RuntimeRevision, error) {
	return f(ctx, revision)
}

type controllerBackendStub struct {
	createCalls int
}

func (b *controllerBackendStub) Create(context.Context, *apiv1alpha1.AgentInstance) (runtimebackend.Endpoint, error) {
	b.createCalls++
	return runtimebackend.Endpoint{A2AAuthority: "runtime.internal"}, nil
}

func (*controllerBackendStub) Fork(context.Context, *apiv1alpha1.AgentInstance, *dbpkg.AgentInstanceCheckpoint) (runtimebackend.Endpoint, error) {
	return runtimebackend.Endpoint{}, nil
}

func (*controllerBackendStub) Quiesce(context.Context, *apiv1alpha1.AgentInstance) (*dbpkg.AgentInstanceTaskSnapshot, error) {
	return &dbpkg.AgentInstanceTaskSnapshot{}, nil
}

func (*controllerBackendStub) Suspend(context.Context, *apiv1alpha1.AgentInstance) error { return nil }
func (*controllerBackendStub) Resume(context.Context, *apiv1alpha1.AgentInstance) error  { return nil }
func (*controllerBackendStub) Delete(context.Context, *apiv1alpha1.AgentInstance) error  { return nil }

func (*controllerBackendStub) Dial(context.Context, *apiv1alpha1.AgentInstance) (*a2aclient.Client, error) {
	return &a2aclient.Client{}, nil
}

func TestDisabledExternalGatewayBuildsSubstrateOnlyRouter(t *testing.T) {
	store := controllerRevisionStoreFunc(func(_ context.Context, revision string) (*dbpkg.RuntimeRevision, error) {
		switch revision {
		case "existing-substrate-revision":
			return &dbpkg.RuntimeRevision{BackendKind: dbpkg.RuntimeBackendKindSubstrate}, nil
		case "external-revision":
			return &dbpkg.RuntimeRevision{BackendKind: dbpkg.RuntimeBackendKindExternal, ExternalRuntime: dbpkg.ExternalRuntimeCodex}, nil
		default:
			return nil, nil
		}
	})
	substrate := &controllerBackendStub{}
	router, err := newRuntimeBackendRouter(store, runtimebackend.Backend{Lifecycle: substrate, Connector: substrate}, nil)
	if err != nil {
		t.Fatalf("build substrate-only router: %v", err)
	}

	endpoint, err := router.Create(t.Context(), &apiv1alpha1.AgentInstance{Id: "existing-instance", PreparedRevision: "existing-substrate-revision"})
	if err != nil {
		t.Fatalf("route existing substrate revision: %v", err)
	}
	if endpoint.A2AAuthority != "runtime.internal" || substrate.createCalls != 1 {
		t.Fatalf("substrate backend was not selected: endpoint=%+v calls=%d", endpoint, substrate.createCalls)
	}

	_, err = router.Create(t.Context(), &apiv1alpha1.AgentInstance{Id: "external-instance", PreparedRevision: "external-revision"})
	if err == nil || !strings.Contains(err.Error(), `backend "external"`) {
		t.Fatalf("disabled external backend did not fail closed: %v", err)
	}
	if substrate.createCalls != 1 {
		t.Fatal("external revision fell back to substrate")
	}
}

func TestEnabledExternalGatewayAddsExplicitExternalBackend(t *testing.T) {
	store := controllerRevisionStoreFunc(func(context.Context, string) (*dbpkg.RuntimeRevision, error) {
		return &dbpkg.RuntimeRevision{BackendKind: dbpkg.RuntimeBackendKindExternal, ExternalRuntime: dbpkg.ExternalRuntimeClaude}, nil
	})
	substrate := &controllerBackendStub{}
	external := &controllerBackendStub{}
	externalBackend := &runtimebackend.Backend{Lifecycle: external, Connector: external}
	router, err := newRuntimeBackendRouter(
		store,
		runtimebackend.Backend{Lifecycle: substrate, Connector: substrate},
		externalBackend,
	)
	if err != nil {
		t.Fatalf("build complete router: %v", err)
	}
	if _, err := router.Create(t.Context(), &apiv1alpha1.AgentInstance{Id: "external-instance", PreparedRevision: "external-revision"}); err != nil {
		t.Fatalf("route external revision: %v", err)
	}
	if external.createCalls != 1 || substrate.createCalls != 0 {
		t.Fatalf("wrong backend selected: external=%d substrate=%d", external.createCalls, substrate.createCalls)
	}
}
