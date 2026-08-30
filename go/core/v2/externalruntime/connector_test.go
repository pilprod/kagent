package externalruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/v2/externalgateway"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const externalClientToken = "Bearer external-client"

func TestAuthorityCodec(t *testing.T) {
	slot := externalgateway.SlotKey{
		DeviceID: "device-one",
		SlotID:   "slot-a",
		Runtime:  externalgateway.RuntimeCodex,
	}

	authority, err := EncodeAuthority(slot)
	require.NoError(t, err)
	assert.Equal(t, "device-one.slot-a.codex", authority)
	decoded, err := DecodeAuthority(authority)
	require.NoError(t, err)
	assert.Equal(t, slot, decoded)
}

func TestDecodeAuthorityRejectsNonSlotData(t *testing.T) {
	invalidAuthorities := []string{
		"",
		"device.slot",
		"device.slot.codex.extra",
		"Device.slot.codex",
		"device_name.slot.codex",
		"device..codex",
		"-device.slot.codex",
		"device.slot-.codex",
		"device.slot.runtime",
		"https://device.slot.codex",
		"user:super-secret@device.slot.codex",
		strings.Repeat("a", 64) + ".slot.codex",
	}

	for _, authority := range invalidAuthorities {
		t.Run(authority, func(t *testing.T) {
			slot, err := DecodeAuthority(authority)
			require.ErrorIs(t, err, ErrInvalidAuthority)
			assert.Empty(t, slot)
			assert.NotContains(t, err.Error(), "super-secret")
		})
	}
}

func TestConnectorRoutesSendMessageToExactSlot(t *testing.T) {
	tests := []struct {
		name         string
		runtime      externalgateway.Runtime
		otherRuntime externalgateway.Runtime
		wantPath     string
	}{
		{name: "codex", runtime: externalgateway.RuntimeCodex, otherRuntime: externalgateway.RuntimeClaude, wantPath: "/codex/v1/invoke"},
		{name: "claude", runtime: externalgateway.RuntimeClaude, otherRuntime: externalgateway.RuntimeCodex, wantPath: "/claude/v1/invoke"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selectedSlot := externalgateway.SlotKey{DeviceID: "device-one", SlotID: "slot-a", Runtime: tt.runtime}
			otherSlot := externalgateway.SlotKey{DeviceID: "device-one", SlotID: "slot-a", Runtime: tt.otherRuntime}
			environment := newExternalEnvironment(t, selectedSlot, otherSlot)
			selectedSession := environment.connect(t, selectedSlot)
			otherSession := environment.connect(t, otherSlot)
			client := environment.dial(t, selectedSlot)

			result := sendMessageAsync(t.Context(), client)
			status, _ := environment.poll(t, otherSession)
			assert.Equal(t, http.StatusNoContent, status, "same device and slot with a different runtime must not receive the request")

			frame := environment.pollForFrame(t, selectedSession)
			assert.Equal(t, externalgateway.FrameTypeRequest, frame.Type)
			assert.Equal(t, http.MethodPost, frame.Method)
			assert.Equal(t, tt.wantPath, frame.Path)
			requestEnvelope := decodeRPCRequest(t, frame.Body)
			assert.Equal(t, "2.0", requestEnvelope.JSONRPC)
			assert.Equal(t, "SendMessage", requestEnvelope.Method)

			responseBody := encodeRPCMessageResponse(t, requestEnvelope.ID, "message-1")
			status = environment.complete(t, selectedSession, frame, responseBody)
			require.Equal(t, http.StatusNoContent, status)
			outcome := <-result
			require.NoError(t, outcome.err)
			message, ok := outcome.result.(*a2a.Message)
			require.True(t, ok)
			assert.Equal(t, "message-1", message.ID)
			assert.Equal(t, a2a.MessageRoleAgent, message.Role)
		})
	}
}

func TestConnectorPreservesOfflineError(t *testing.T) {
	slot := externalgateway.SlotKey{DeviceID: "device-one", SlotID: "slot-a", Runtime: externalgateway.RuntimeCodex}
	environment := newExternalEnvironment(t, slot)
	client := environment.dial(t, slot)

	result, err := client.SendMessage(t.Context(), newSendMessageRequest())
	assert.Nil(t, result)
	require.ErrorIs(t, err, externalgateway.ErrOffline)
	assert.NotErrorIs(t, err, externalgateway.ErrUnknownOutcome)
}

func TestConnectorPreservesUnknownOutcomeWithoutRetry(t *testing.T) {
	slot := externalgateway.SlotKey{DeviceID: "device-one", SlotID: "slot-a", Runtime: externalgateway.RuntimeCodex}
	environment := newExternalEnvironment(t, slot)
	firstSession := environment.connect(t, slot)
	client := environment.dial(t, slot)
	result := sendMessageAsync(t.Context(), client)

	environment.pollForFrame(t, firstSession)
	secondSession := environment.connect(t, slot)
	outcome := <-result
	assert.Nil(t, outcome.result)
	require.ErrorIs(t, outcome.err, externalgateway.ErrUnknownOutcome)
	assert.NotErrorIs(t, outcome.err, externalgateway.ErrOffline)

	status, _ := environment.poll(t, secondSession)
	assert.Equal(t, http.StatusNoContent, status, "connector must not retry an unknown outcome")
}

func TestBrokerRoundTripperRemovesSecretHeaders(t *testing.T) {
	slot := externalgateway.SlotKey{DeviceID: "device-one", SlotID: "slot-a", Runtime: externalgateway.RuntimeCodex}
	environment := newExternalEnvironment(t, slot)
	session := environment.connect(t, slot)
	path, err := invokePath(slot.Runtime)
	require.NoError(t, err)
	transport := &brokerRoundTripper{broker: environment.broker, slot: slot, invokePath: path}

	type secretContextKey struct{}
	ctx := context.WithValue(t.Context(), secretContextKey{}, "context-secret")
	body := &trackingReadCloser{Reader: strings.NewReader(`{"jsonrpc":"2.0"}`)}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://must-not-reach-client.invalid"+path, body)
	require.NoError(t, err)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-A2A-Extensions", "extension-one")
	request.Header.Set("Authorization", "Bearer user-secret")
	request.Header.Set("Proxy-Authorization", "Bearer proxy-secret")
	request.Header.Set("Cookie", "session=browser-secret")
	request.Header.Set("Traceparent", "00-trace-secret")
	request.Header.Set("Tracestate", "vendor=trace-secret")
	request.Header.Set("Baggage", "temporal-token=secret")
	request.Header.Set("X-Temporal-Namespace", "secret-namespace")
	request.Header.Set("X-A2A-Tenant", "secret-tenant")

	roundTrip := roundTripAsync(transport, request)
	frame := environment.pollForFrame(t, session)
	assert.EqualValues(t, 1, body.closeCalls.Load())
	assert.Equal(t, path, frame.Path, "successful dispatch proves scheme and host were removed")
	assert.Equal(t, "application/json", frame.Headers.Get("Accept"))
	assert.Equal(t, "application/json", frame.Headers.Get("Content-Type"))
	assert.Equal(t, "extension-one", frame.Headers.Get("X-A2A-Extensions"))
	for _, name := range []string{
		"Authorization",
		"Proxy-Authorization",
		"Cookie",
		"Traceparent",
		"Tracestate",
		"Baggage",
		"X-Temporal-Namespace",
		"X-A2A-Tenant",
	} {
		assert.Empty(t, frame.Headers.Values(name), "%s must not reach the external client", name)
	}
	assert.Len(t, frame.Headers, 3)

	status := environment.complete(t, session, frame, []byte(`{}`))
	require.Equal(t, http.StatusNoContent, status)
	outcome := <-roundTrip
	require.NoError(t, outcome.err)
	require.NotNil(t, outcome.response)
	require.NoError(t, outcome.response.Body.Close())
	assert.EqualValues(t, 1, body.closeCalls.Load())
}

func TestBrokerRoundTripperClosesRejectedRequestBodyOnce(t *testing.T) {
	slot := externalgateway.SlotKey{DeviceID: "device-one", SlotID: "slot-a", Runtime: externalgateway.RuntimeCodex}
	environment := newExternalEnvironment(t, slot)
	path, err := invokePath(slot.Runtime)
	require.NoError(t, err)
	transport := &brokerRoundTripper{broker: environment.broker, slot: slot, invokePath: path}
	closeErr := errors.New("private close failure")
	body := &trackingReadCloser{Reader: strings.NewReader(`{}`), closeErr: closeErr}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://super-secret.invalid/not-the-invoke-path", body)
	require.NoError(t, err)

	response, err := transport.RoundTrip(request)
	assert.Nil(t, response)
	require.ErrorIs(t, err, externalgateway.ErrInvalidRequest)
	assert.ErrorIs(t, err, closeErr)
	assert.NotContains(t, err.Error(), "super-secret.invalid")
	assert.NotContains(t, err.Error(), "private close failure")
	assert.EqualValues(t, 1, body.closeCalls.Load())
}

func TestProtocolHeadersRemoveSecretsBeforeBroker(t *testing.T) {
	source := http.Header{
		"Accept":               []string{"application/json"},
		"Content-Type":         []string{"application/json"},
		"X-A2a-Extensions":     []string{"extension-one"},
		"Authorization":        []string{"Bearer user-secret"},
		"Cookie":               []string{"session=browser-secret"},
		"Traceparent":          []string{"00-trace-secret"},
		"Baggage":              []string{"temporal-token=secret"},
		"X-Temporal-Namespace": []string{"secret-namespace"},
	}

	got := protocolHeaders(source)
	assert.Equal(t, "application/json", got.Get("Accept"))
	assert.Equal(t, "application/json", got.Get("Content-Type"))
	assert.Equal(t, "extension-one", got.Get("X-A2A-Extensions"))
	assert.Len(t, got, 3)
	for _, name := range []string{"Authorization", "Cookie", "Traceparent", "Baggage", "X-Temporal-Namespace"} {
		assert.Empty(t, got.Values(name))
	}
}

func TestTransportContextDropsValuesAndPreservesCancellation(t *testing.T) {
	type secretContextKey struct{}
	parent, cancel := context.WithCancel(context.WithValue(t.Context(), secretContextKey{}, "temporal-secret"))
	transportCtx := &transportContext{Context: parent}

	assert.Nil(t, transportCtx.Value(secretContextKey{}))
	cancel()
	require.ErrorIs(t, transportCtx.Err(), context.Canceled)
	select {
	case <-transportCtx.Done():
	default:
		t.Fatal("transport context did not preserve cancellation")
	}
}

func TestConnectorRejectsInvalidAuthorityWithoutLeakingIt(t *testing.T) {
	slot := externalgateway.SlotKey{DeviceID: "device-one", SlotID: "slot-a", Runtime: externalgateway.RuntimeCodex}
	environment := newExternalEnvironment(t, slot)
	connector, err := NewConnector(environment.broker)
	require.NoError(t, err)

	client, err := connector.Dial(t.Context(), &apiv1alpha1.AgentInstance{
		Id:           "instance-1",
		A2AAuthority: "user:super-secret@device.slot.codex",
	})
	assert.Nil(t, client)
	require.ErrorIs(t, err, ErrInvalidAuthority)
	assert.NotContains(t, err.Error(), "super-secret")
}

func TestNewConnectorRequiresBroker(t *testing.T) {
	connector, err := NewConnector(nil)
	require.ErrorContains(t, err, "broker is nil")
	assert.Nil(t, connector)
}

type rpcRequestEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	ID      json.RawMessage `json:"id"`
}

type sendMessageOutcome struct {
	result a2a.SendMessageResult
	err    error
}

type roundTripOutcome struct {
	response *http.Response
	err      error
}

type trackingReadCloser struct {
	io.Reader
	closeErr   error
	closeCalls atomic.Int64
}

func (b *trackingReadCloser) Close() error {
	b.closeCalls.Add(1)
	return b.closeErr
}

type staticAuthenticator struct {
	claims externalgateway.Claims
}

func (a *staticAuthenticator) Authenticate(_ context.Context, headers http.Header) (externalgateway.Claims, error) {
	if headers.Get("Authorization") != externalClientToken {
		return externalgateway.Claims{}, errors.New("invalid external client token")
	}
	return a.claims, nil
}

type externalEnvironment struct {
	broker    *externalgateway.Broker
	serverURL string
	client    *http.Client
}

func newExternalEnvironment(t *testing.T, slots ...externalgateway.SlotKey) *externalEnvironment {
	t.Helper()
	broker, err := externalgateway.NewBroker(externalgateway.Config{
		PollTimeout:           20 * time.Millisecond,
		HeartbeatTimeout:      2 * time.Second,
		RequestTimeout:        2 * time.Second,
		MaxBodyBytes:          1 << 20,
		MaxHeaderBytes:        4096,
		MaxHeaderCount:        16,
		MaxAllowedPaths:       4,
		MaxConcurrencyPerSlot: 4,
	}, &staticAuthenticator{claims: externalgateway.Claims{Subject: "external-client", AllowedSlots: slots}})
	require.NoError(t, err)
	server := httptest.NewServer(broker)
	t.Cleanup(server.Close)
	return &externalEnvironment{broker: broker, serverURL: server.URL, client: server.Client()}
}

func (e *externalEnvironment) dial(t *testing.T, slot externalgateway.SlotKey) *a2aclient.Client {
	t.Helper()
	authority, err := EncodeAuthority(slot)
	require.NoError(t, err)
	connector, err := NewConnector(e.broker)
	require.NoError(t, err)
	client, err := connector.Dial(t.Context(), &apiv1alpha1.AgentInstance{Id: "instance-1", A2AAuthority: authority})
	require.NoError(t, err)
	return client
}

func (e *externalEnvironment) connect(t *testing.T, slot externalgateway.SlotKey) externalgateway.ConnectResponse {
	t.Helper()
	path, err := invokePath(slot.Runtime)
	require.NoError(t, err)
	response := e.post(t, externalgateway.ConnectPath, externalgateway.ConnectRequest{
		ProtocolVersion: externalgateway.ProtocolVersion,
		DeviceID:        slot.DeviceID,
		SlotID:          slot.SlotID,
		Runtime:         slot.Runtime,
		MaxConcurrency:  1,
		AllowedPaths:    []string{path},
	})
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode, responseBody(t, response))
	var connected externalgateway.ConnectResponse
	require.NoError(t, json.NewDecoder(response.Body).Decode(&connected))
	return connected
}

func (e *externalEnvironment) poll(t *testing.T, session externalgateway.ConnectResponse) (int, externalgateway.Frame) {
	t.Helper()
	response := e.post(t, externalgateway.PollPath, externalgateway.PollRequest{
		SessionID: session.SessionID, Generation: session.Generation,
	})
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return response.StatusCode, externalgateway.Frame{}
	}
	var frame externalgateway.Frame
	require.NoError(t, json.NewDecoder(response.Body).Decode(&frame))
	return response.StatusCode, frame
}

func (e *externalEnvironment) pollForFrame(t *testing.T, session externalgateway.ConnectResponse) externalgateway.Frame {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status, frame := e.poll(t, session)
		switch status {
		case http.StatusOK:
			return frame
		case http.StatusNoContent:
			continue
		default:
			require.Equal(t, http.StatusOK, status)
		}
	}
	t.Fatal("external runtime request was not dispatched")
	return externalgateway.Frame{}
}

func (e *externalEnvironment) complete(t *testing.T, session externalgateway.ConnectResponse, frame externalgateway.Frame, body []byte) int {
	t.Helper()
	response := e.post(t, externalgateway.CompletePath, externalgateway.CompleteRequest{
		SessionID:  session.SessionID,
		Generation: session.Generation,
		RequestID:  frame.RequestID,
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       body,
	})
	defer response.Body.Close()
	return response.StatusCode
}

func (e *externalEnvironment) post(t *testing.T, path string, input any) *http.Response {
	t.Helper()
	payload, err := json.Marshal(input)
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.serverURL+path, bytes.NewReader(payload))
	require.NoError(t, err)
	request.Header.Set("Authorization", externalClientToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := e.client.Do(request)
	require.NoError(t, err)
	return response
}

func sendMessageAsync(ctx context.Context, client *a2aclient.Client) <-chan sendMessageOutcome {
	result := make(chan sendMessageOutcome, 1)
	go func() {
		message, err := client.SendMessage(ctx, newSendMessageRequest())
		result <- sendMessageOutcome{result: message, err: err}
	}()
	return result
}

func newSendMessageRequest() *a2a.SendMessageRequest {
	return &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello")),
	}
}

func decodeRPCRequest(t *testing.T, body []byte) rpcRequestEnvelope {
	t.Helper()
	var envelope rpcRequestEnvelope
	require.NoError(t, json.Unmarshal(body, &envelope))
	require.NotEmpty(t, envelope.ID)
	return envelope
}

func encodeRPCMessageResponse(t *testing.T, id json.RawMessage, messageID string) []byte {
	t.Helper()
	response := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Result:  json.RawMessage(`{"message":{"messageId":"` + messageID + `","role":"ROLE_AGENT","parts":[{"text":"done"}]}}`),
	}
	payload, err := json.Marshal(response)
	require.NoError(t, err)
	return payload
}

func roundTripAsync(transport http.RoundTripper, request *http.Request) <-chan roundTripOutcome {
	result := make(chan roundTripOutcome, 1)
	go func() {
		response, err := transport.RoundTrip(request)
		result <- roundTripOutcome{response: response, err: err}
	}()
	return result
}

func responseBody(t *testing.T, response *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	response.Body = io.NopCloser(bytes.NewReader(body))
	return string(body)
}
