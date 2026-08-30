package externalruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/v2/externalgateway"
	"github.com/kagent-dev/kagent/go/core/v2/runtimebackend"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const lifecycleRevisionID = "runtime-revision-one"

type lifecycleRevisionStore struct {
	revision *dbpkg.RuntimeRevision
	err      error
	calls    int
}

func (s *lifecycleRevisionStore) GetRuntimeRevision(context.Context, string) (*dbpkg.RuntimeRevision, error) {
	s.calls++
	return s.revision, s.err
}

type createOutcome struct {
	endpoint runtimebackend.Endpoint
	err      error
}

func TestLifecycleCreateProbesExactPlacedSlot(t *testing.T) {
	selected := externalgateway.SlotKey{DeviceID: "device-one", SlotID: "slot-selected", Runtime: externalgateway.RuntimeCodex}
	other := externalgateway.SlotKey{DeviceID: "device-one", SlotID: "slot-other", Runtime: externalgateway.RuntimeCodex}
	environment := newExternalEnvironment(t, selected, other)
	selectedSession := environment.connectPaths(t, selected, []string{mustAgentCardPath(t, dbpkg.ExternalRuntimeCodex)})
	otherSession := environment.connectPaths(t, other, []string{mustAgentCardPath(t, dbpkg.ExternalRuntimeCodex)})
	lifecycle := newTestLifecycle(t, environment.broker, externalRevision(dbpkg.ExternalRuntimeCodex), map[dbpkg.ExternalRuntime]externalgateway.SlotKey{
		dbpkg.ExternalRuntimeCodex: selected,
	})

	outcome := createAsync(t.Context(), lifecycle, externalInstance(""))
	status, _ := environment.poll(t, otherSession)
	assert.Equal(t, http.StatusNoContent, status, "an unplaced slot must not receive the probe")
	frame := environment.pollForFrame(t, selectedSession)
	assert.Equal(t, externalgateway.FrameTypeRequest, frame.Type)
	assert.Equal(t, http.MethodGet, frame.Method)
	assert.Equal(t, "/codex/v1/.well-known/agent-card.json", frame.Path)
	assert.Equal(t, "application/json", frame.Headers.Get("Accept"))
	require.Equal(t, http.StatusNoContent, environment.complete(t, selectedSession, frame, validAgentCard(t, dbpkg.ExternalRuntimeCodex)))

	result := <-outcome
	require.NoError(t, result.err)
	assert.Equal(t, runtimebackend.Endpoint{A2AAuthority: "device-one.slot-selected.codex"}, result.endpoint)
}

func TestAgentCardPathIsFixedPerRuntime(t *testing.T) {
	tests := map[dbpkg.ExternalRuntime]string{
		dbpkg.ExternalRuntimeCodex:  "/codex/v1/.well-known/agent-card.json",
		dbpkg.ExternalRuntimeClaude: "/claude/v1/.well-known/agent-card.json",
	}
	for runtime, want := range tests {
		got, err := agentCardPath(runtime)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
}

func TestLifecycleCreateAndResumePreserveOfflineError(t *testing.T) {
	slot := externalgateway.SlotKey{DeviceID: "device-one", SlotID: "slot-a", Runtime: externalgateway.RuntimeCodex}
	environment := newExternalEnvironment(t, slot)
	lifecycle := newTestLifecycle(t, environment.broker, externalRevision(dbpkg.ExternalRuntimeCodex), map[dbpkg.ExternalRuntime]externalgateway.SlotKey{
		dbpkg.ExternalRuntimeCodex: slot,
	})

	_, err := lifecycle.Create(t.Context(), externalInstance(""))
	require.ErrorIs(t, err, externalgateway.ErrOffline)
	assert.NotContains(t, err.Error(), "device-one.slot-a.codex")

	err = lifecycle.Resume(t.Context(), externalInstance("device-one.slot-a.codex"))
	require.ErrorIs(t, err, externalgateway.ErrOffline)
	assert.NotContains(t, err.Error(), "device-one.slot-a.codex")
}

func TestLifecycleCreateRejectsNilInstanceAndNilRevision(t *testing.T) {
	slot := externalgateway.SlotKey{DeviceID: "device-one", SlotID: "slot-a", Runtime: externalgateway.RuntimeCodex}
	environment := newExternalEnvironment(t, slot)
	lifecycle := newTestLifecycle(t, environment.broker, externalRevision(dbpkg.ExternalRuntimeCodex), map[dbpkg.ExternalRuntime]externalgateway.SlotKey{
		dbpkg.ExternalRuntimeCodex: slot,
	})

	endpoint, err := lifecycle.Create(t.Context(), nil)
	assert.Empty(t, endpoint)
	require.Error(t, err)

	lifecycle = newTestLifecycle(t, environment.broker, nil, map[dbpkg.ExternalRuntime]externalgateway.SlotKey{
		dbpkg.ExternalRuntimeCodex: slot,
	})
	endpoint, err = lifecycle.Create(t.Context(), externalInstance(""))
	assert.Empty(t, endpoint)
	require.Error(t, err)
}

func TestLifecycleDeleteDoesNotTerminateSharedRuntime(t *testing.T) {
	slot := externalgateway.SlotKey{DeviceID: "device-one", SlotID: "slot-a", Runtime: externalgateway.RuntimeCodex}
	environment := newExternalEnvironment(t, slot)
	cardPath := mustAgentCardPath(t, dbpkg.ExternalRuntimeCodex)
	session := environment.connectPaths(t, slot, []string{cardPath})
	lifecycle := newTestLifecycle(t, environment.broker, externalRevision(dbpkg.ExternalRuntimeCodex), map[dbpkg.ExternalRuntime]externalgateway.SlotKey{
		dbpkg.ExternalRuntimeCodex: slot,
	})
	instance := externalInstance("device-one.slot-a.codex")

	require.NoError(t, lifecycle.Delete(t.Context(), instance))
	require.NoError(t, lifecycle.Delete(t.Context(), instance), "repeated logical deletion must converge")
	status, _ := environment.poll(t, session)
	assert.Equal(t, http.StatusNoContent, status, "logical deletion must not dispatch a client operation")

	resume := resumeAsync(t.Context(), lifecycle, instance)
	frame := environment.pollForFrame(t, session)
	assert.Equal(t, cardPath, frame.Path, "the same connected session must survive logical deletion")
	require.Equal(t, http.StatusNoContent, environment.complete(t, session, frame, validAgentCard(t, dbpkg.ExternalRuntimeCodex)))
	require.NoError(t, <-resume)
}

func TestLifecycleSuspendAndDeleteWithoutAuthorityDoNotLoadRevision(t *testing.T) {
	slot := externalgateway.SlotKey{DeviceID: "device-one", SlotID: "slot-a", Runtime: externalgateway.RuntimeCodex}
	environment := newExternalEnvironment(t, slot)
	placement, err := NewStaticPlacement(map[dbpkg.ExternalRuntime]externalgateway.SlotKey{dbpkg.ExternalRuntimeCodex: slot})
	require.NoError(t, err)
	store := &lifecycleRevisionStore{err: errors.New("revision store must not be called")}
	lifecycle, err := NewLifecycle(store, placement, environment.broker, time.Second)
	require.NoError(t, err)
	instance := externalInstance("")

	require.NoError(t, lifecycle.Suspend(t.Context(), instance))
	require.NoError(t, lifecycle.Delete(t.Context(), instance))
	assert.Zero(t, store.calls)
}

func TestLifecycleRejectsInvalidAgentCards(t *testing.T) {
	tests := []struct {
		name string
		card func(*testing.T) []byte
	}{
		{
			name: "malformed JSON",
			card: func(*testing.T) []byte { return []byte(`{"name":`) },
		},
		{
			name: "streaming enabled",
			card: func(t *testing.T) []byte {
				card := testAgentCard(dbpkg.ExternalRuntimeCodex)
				card.Capabilities.Streaming = true
				return marshalAgentCard(t, card)
			},
		},
		{
			name: "not A2A v1",
			card: func(t *testing.T) []byte {
				card := testAgentCard(dbpkg.ExternalRuntimeCodex)
				card.SupportedInterfaces[0].ProtocolVersion = "0.3"
				return marshalAgentCard(t, card)
			},
		},
		{
			name: "not JSON-RPC",
			card: func(t *testing.T) []byte {
				card := testAgentCard(dbpkg.ExternalRuntimeCodex)
				card.SupportedInterfaces[0].ProtocolBinding = a2atype.TransportProtocolHTTPJSON
				return marshalAgentCard(t, card)
			},
		},
		{
			name: "missing interface URL",
			card: func(t *testing.T) []byte {
				card := testAgentCard(dbpkg.ExternalRuntimeCodex)
				card.SupportedInterfaces[0].URL = ""
				return marshalAgentCard(t, card)
			},
		},
		{
			name: "missing required modes",
			card: func(t *testing.T) []byte {
				card := testAgentCard(dbpkg.ExternalRuntimeCodex)
				card.DefaultOutputModes = nil
				return marshalAgentCard(t, card)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slot := externalgateway.SlotKey{DeviceID: "device-one", SlotID: "slot-a", Runtime: externalgateway.RuntimeCodex}
			environment := newExternalEnvironment(t, slot)
			session := environment.connectPaths(t, slot, []string{mustAgentCardPath(t, dbpkg.ExternalRuntimeCodex)})
			lifecycle := newTestLifecycle(t, environment.broker, externalRevision(dbpkg.ExternalRuntimeCodex), map[dbpkg.ExternalRuntime]externalgateway.SlotKey{
				dbpkg.ExternalRuntimeCodex: slot,
			})

			outcome := createAsync(t.Context(), lifecycle, externalInstance(""))
			frame := environment.pollForFrame(t, session)
			require.Equal(t, http.StatusNoContent, environment.complete(t, session, frame, tt.card(t)))
			result := <-outcome
			assert.Empty(t, result.endpoint)
			require.Error(t, result.err)
			assert.NotErrorIs(t, result.err, externalgateway.ErrOffline)
		})
	}
}

func TestAgentCardResponseRejectsCloseErrorContentTypeAndOversizedBody(t *testing.T) {
	validBody := validAgentCard(t, dbpkg.ExternalRuntimeCodex)
	closeErr := errors.New("private body close failure")
	tests := []struct {
		name        string
		contentType string
		body        []byte
		closeErr    error
		wantErrorIs error
	}{
		{
			name:        "body close error",
			contentType: "application/json",
			body:        validBody,
			closeErr:    closeErr,
			wantErrorIs: closeErr,
		},
		{
			name:        "non-JSON content type",
			contentType: "text/plain",
			body:        validBody,
		},
		{
			name:        "oversized body",
			contentType: "application/json",
			body:        bytes.Repeat([]byte("x"), int(maxAgentCardBytes+1)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := &trackingReadCloser{Reader: bytes.NewReader(tt.body), closeErr: tt.closeErr}
			err := validateAgentCardResponse(&http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{tt.contentType}},
				Body:       body,
			})
			require.Error(t, err)
			if tt.wantErrorIs != nil {
				assert.ErrorIs(t, err, tt.wantErrorIs)
			}
			assert.EqualValues(t, 1, body.closeCalls.Load())
		})
	}
}

func TestLifecycleRejectsAuthorityAndRevisionMismatchBeforeProbe(t *testing.T) {
	slot := externalgateway.SlotKey{DeviceID: "device-one", SlotID: "slot-a", Runtime: externalgateway.RuntimeCodex}
	environment := newExternalEnvironment(t, slot)
	placement := map[dbpkg.ExternalRuntime]externalgateway.SlotKey{dbpkg.ExternalRuntimeCodex: slot}

	tests := []struct {
		name      string
		revision  *dbpkg.RuntimeRevision
		authority string
	}{
		{
			name:      "authority slot",
			revision:  externalRevision(dbpkg.ExternalRuntimeCodex),
			authority: "device-one.slot-other.codex",
		},
		{
			name: "revision identity",
			revision: &dbpkg.RuntimeRevision{
				Revision:        "another-revision",
				BackendKind:     dbpkg.RuntimeBackendKindExternal,
				ExternalRuntime: dbpkg.ExternalRuntimeCodex,
			},
			authority: "device-one.slot-a.codex",
		},
		{
			name: "backend kind",
			revision: &dbpkg.RuntimeRevision{
				Revision:    lifecycleRevisionID,
				BackendKind: dbpkg.RuntimeBackendKindSubstrate,
			},
			authority: "device-one.slot-a.codex",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lifecycle := newTestLifecycle(t, environment.broker, tt.revision, placement)
			err := lifecycle.Resume(t.Context(), externalInstance(tt.authority))
			require.Error(t, err)
			assert.NotErrorIs(t, err, externalgateway.ErrOffline, "mismatches must fail before contacting the broker")
			assert.NotContains(t, err.Error(), tt.authority)
		})
	}
}

func TestLifecycleCheckpointOperationsAreExplicitlyUnsupported(t *testing.T) {
	lifecycle := &Lifecycle{}
	instance := externalInstance("device-one.slot-a.codex")

	endpoint, err := lifecycle.Fork(t.Context(), instance, &dbpkg.AgentInstanceCheckpoint{})
	assert.Empty(t, endpoint)
	require.ErrorIs(t, err, runtimebackend.ErrCheckpointUnsupported)

	snapshot, err := lifecycle.Quiesce(t.Context(), instance)
	assert.Nil(t, snapshot)
	require.ErrorIs(t, err, runtimebackend.ErrCheckpointUnsupported)
}

func TestStaticPlacementIsImmutableAndHasNoFallback(t *testing.T) {
	selected := externalgateway.SlotKey{DeviceID: "device-one", SlotID: "slot-a", Runtime: externalgateway.RuntimeCodex}
	mutated := externalgateway.SlotKey{DeviceID: "device-two", SlotID: "slot-b", Runtime: externalgateway.RuntimeCodex}
	slots := map[dbpkg.ExternalRuntime]externalgateway.SlotKey{dbpkg.ExternalRuntimeCodex: selected}
	placement, err := NewStaticPlacement(slots)
	require.NoError(t, err)

	slots[dbpkg.ExternalRuntimeCodex] = mutated
	slots[dbpkg.ExternalRuntimeClaude] = externalgateway.SlotKey{DeviceID: "device-two", SlotID: "slot-c", Runtime: externalgateway.RuntimeClaude}
	got, err := placement.Select(dbpkg.ExternalRuntimeCodex)
	require.NoError(t, err)
	assert.Equal(t, selected, got)

	got, err = placement.Select(dbpkg.ExternalRuntimeClaude)
	require.Error(t, err)
	assert.Empty(t, got)
}

func newTestLifecycle(
	t *testing.T,
	broker *externalgateway.Broker,
	revision *dbpkg.RuntimeRevision,
	slots map[dbpkg.ExternalRuntime]externalgateway.SlotKey,
) *Lifecycle {
	t.Helper()
	placement, err := NewStaticPlacement(slots)
	require.NoError(t, err)
	lifecycle, err := NewLifecycle(&lifecycleRevisionStore{revision: revision}, placement, broker, time.Second)
	require.NoError(t, err)
	return lifecycle
}

func externalRevision(runtime dbpkg.ExternalRuntime) *dbpkg.RuntimeRevision {
	return &dbpkg.RuntimeRevision{
		Revision:        lifecycleRevisionID,
		BackendKind:     dbpkg.RuntimeBackendKindExternal,
		ExternalRuntime: runtime,
	}
}

func externalInstance(authority string) *apiv1alpha1.AgentInstance {
	return &apiv1alpha1.AgentInstance{
		Id:               "instance-one",
		PreparedRevision: lifecycleRevisionID,
		A2AAuthority:     authority,
	}
}

func (e *externalEnvironment) connectPaths(t *testing.T, slot externalgateway.SlotKey, paths []string) externalgateway.ConnectResponse {
	t.Helper()
	response := e.post(t, externalgateway.ConnectPath, externalgateway.ConnectRequest{
		ProtocolVersion: externalgateway.ProtocolVersion,
		DeviceID:        slot.DeviceID,
		SlotID:          slot.SlotID,
		Runtime:         slot.Runtime,
		MaxConcurrency:  1,
		AllowedPaths:    paths,
	})
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode, responseBody(t, response))
	var connected externalgateway.ConnectResponse
	require.NoError(t, json.NewDecoder(response.Body).Decode(&connected))
	return connected
}

func createAsync(ctx context.Context, lifecycle *Lifecycle, instance *apiv1alpha1.AgentInstance) <-chan createOutcome {
	result := make(chan createOutcome, 1)
	go func() {
		endpoint, err := lifecycle.Create(ctx, instance)
		result <- createOutcome{endpoint: endpoint, err: err}
	}()
	return result
}

func resumeAsync(ctx context.Context, lifecycle *Lifecycle, instance *apiv1alpha1.AgentInstance) <-chan error {
	result := make(chan error, 1)
	go func() {
		result <- lifecycle.Resume(ctx, instance)
	}()
	return result
}

func mustAgentCardPath(t *testing.T, runtime dbpkg.ExternalRuntime) string {
	t.Helper()
	path, err := agentCardPath(runtime)
	require.NoError(t, err)
	return path
}

func validAgentCard(t *testing.T, runtime dbpkg.ExternalRuntime) []byte {
	t.Helper()
	return marshalAgentCard(t, testAgentCard(runtime))
}

func testAgentCard(runtime dbpkg.ExternalRuntime) *a2atype.AgentCard {
	brokerRuntime, _ := gatewayRuntime(runtime)
	path, _ := invokePath(brokerRuntime)
	return &a2atype.AgentCard{
		Name:               "local-agent",
		Description:        "External local coding agent",
		Version:            "1",
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             []a2atype.AgentSkill{},
		Capabilities:       a2atype.AgentCapabilities{Streaming: false},
		SupportedInterfaces: []*a2atype.AgentInterface{{
			URL:             "http://127.0.0.1" + path,
			ProtocolBinding: a2atype.TransportProtocolJSONRPC,
			ProtocolVersion: a2atype.Version,
		}},
	}
}

func marshalAgentCard(t *testing.T, card *a2atype.AgentCard) []byte {
	t.Helper()
	body, err := json.Marshal(card)
	require.NoError(t, err)
	return body
}

func TestLifecycleConstructorRejectsNilDependencies(t *testing.T) {
	slot := externalgateway.SlotKey{DeviceID: "device-one", SlotID: "slot-a", Runtime: externalgateway.RuntimeCodex}
	environment := newExternalEnvironment(t, slot)
	placement, err := NewStaticPlacement(map[dbpkg.ExternalRuntime]externalgateway.SlotKey{dbpkg.ExternalRuntimeCodex: slot})
	require.NoError(t, err)

	var nilStore *lifecycleRevisionStore
	_, err = NewLifecycle(nilStore, placement, environment.broker, time.Second)
	require.Error(t, err)
	_, err = NewLifecycle(&lifecycleRevisionStore{}, (*StaticPlacement)(nil), environment.broker, time.Second)
	require.Error(t, err)
	_, err = NewLifecycle(&lifecycleRevisionStore{}, placement, nil, time.Second)
	require.Error(t, err)
	_, err = NewLifecycle(&lifecycleRevisionStore{}, placement, environment.broker, 0)
	require.Error(t, err)
}

func TestLifecycleStoreErrorsRemainDiscoverableButRedacted(t *testing.T) {
	slot := externalgateway.SlotKey{DeviceID: "device-one", SlotID: "slot-a", Runtime: externalgateway.RuntimeCodex}
	environment := newExternalEnvironment(t, slot)
	placement, err := NewStaticPlacement(map[dbpkg.ExternalRuntime]externalgateway.SlotKey{dbpkg.ExternalRuntimeCodex: slot})
	require.NoError(t, err)
	privateErr := errors.New("https://user:credential@runtime.internal")
	lifecycle, err := NewLifecycle(&lifecycleRevisionStore{err: privateErr}, placement, environment.broker, time.Second)
	require.NoError(t, err)

	_, err = lifecycle.Create(t.Context(), externalInstance(""))
	require.ErrorIs(t, err, privateErr)
	assert.NotContains(t, err.Error(), "credential")
}
