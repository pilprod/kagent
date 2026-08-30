package externalgateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/core/v2/externalgateway"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	validToken = "Bearer device-one"
	otherToken = "Bearer device-two"
	invokePath = "/codex/v1/invoke"
	cardPath   = "/codex/v1/.well-known/agent-card.json"
)

var (
	validSlot = externalgateway.SlotKey{DeviceID: "device-one", SlotID: "slot-a", Runtime: externalgateway.RuntimeCodex}
	otherSlot = externalgateway.SlotKey{DeviceID: "device-two", SlotID: "slot-a", Runtime: externalgateway.RuntimeCodex}
)

type tokenAuthenticator struct {
	claims map[string]externalgateway.Claims
}

func (a *tokenAuthenticator) Authenticate(_ context.Context, headers http.Header) (externalgateway.Claims, error) {
	claims, ok := a.claims[headers.Get("Authorization")]
	if !ok {
		return externalgateway.Claims{}, errors.New("invalid token")
	}
	return claims, nil
}

type testEnvironment struct {
	broker *externalgateway.Broker
	server *httptest.Server
	client *http.Client
}

func newTestEnvironment(t *testing.T, mutate func(*externalgateway.Config)) *testEnvironment {
	t.Helper()
	config := externalgateway.Config{
		PollTimeout:           40 * time.Millisecond,
		HeartbeatTimeout:      200 * time.Millisecond,
		RequestTimeout:        2 * time.Second,
		MaxBodyBytes:          1024,
		MaxHeaderBytes:        1024,
		MaxHeaderCount:        16,
		MaxAllowedPaths:       4,
		MaxConcurrencyPerSlot: 4,
	}
	if mutate != nil {
		mutate(&config)
	}
	authenticator := &tokenAuthenticator{claims: map[string]externalgateway.Claims{
		validToken: {Subject: "subject-one", AllowedSlots: []externalgateway.SlotKey{validSlot}},
		otherToken: {Subject: "subject-two", AllowedSlots: []externalgateway.SlotKey{otherSlot}},
	}}
	broker, err := externalgateway.NewBroker(config, authenticator)
	require.NoError(t, err)
	server := httptest.NewServer(broker)
	t.Cleanup(server.Close)
	return &testEnvironment{broker: broker, server: server, client: server.Client()}
}

func (e *testEnvironment) post(t *testing.T, endpoint string, token string, input any) *http.Response {
	t.Helper()
	payload, err := json.Marshal(input)
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPost, e.server.URL+endpoint, bytes.NewReader(payload))
	require.NoError(t, err)
	request.Header.Set("Authorization", token)
	request.Header.Set("Content-Type", "application/json")
	response, err := e.client.Do(request)
	require.NoError(t, err)
	return response
}

func (e *testEnvironment) connect(t *testing.T, token string, slot externalgateway.SlotKey, maxConcurrency int, allowedPaths []string) externalgateway.ConnectResponse {
	t.Helper()
	response := e.post(t, externalgateway.ConnectPath, token, externalgateway.ConnectRequest{
		ProtocolVersion: externalgateway.ProtocolVersion,
		DeviceID:        slot.DeviceID,
		SlotID:          slot.SlotID,
		Runtime:         slot.Runtime,
		MaxConcurrency:  maxConcurrency,
		AllowedPaths:    allowedPaths,
	})
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode, readResponseBody(t, response))
	var connected externalgateway.ConnectResponse
	require.NoError(t, json.NewDecoder(response.Body).Decode(&connected))
	require.NotEmpty(t, connected.SessionID)
	return connected
}

func (e *testEnvironment) poll(t *testing.T, token string, session externalgateway.ConnectResponse) (int, externalgateway.Frame) {
	t.Helper()
	response := e.post(t, externalgateway.PollPath, token, externalgateway.PollRequest{
		SessionID:  session.SessionID,
		Generation: session.Generation,
	})
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return response.StatusCode, externalgateway.Frame{}
	}
	var frame externalgateway.Frame
	require.NoError(t, json.NewDecoder(response.Body).Decode(&frame))
	return response.StatusCode, frame
}

func (e *testEnvironment) complete(t *testing.T, token string, session externalgateway.ConnectResponse, frame externalgateway.Frame, status int, headers http.Header, body []byte) int {
	t.Helper()
	response := e.post(t, externalgateway.CompletePath, token, externalgateway.CompleteRequest{
		SessionID:  session.SessionID,
		Generation: session.Generation,
		RequestID:  frame.RequestID,
		StatusCode: status,
		Headers:    headers,
		Body:       body,
	})
	defer response.Body.Close()
	return response.StatusCode
}

func newForwardRequest(method string, path string, body string) *http.Request {
	return &http.Request{
		Method: method,
		URL:    &url.URL{Path: path},
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}
}

type trackingBody struct {
	*strings.Reader
	closeCount int
}

type blockingBody struct {
	reader      *strings.Reader
	readStarted chan struct{}
	release     chan struct{}
	started     bool
	closeCount  int
}

func (b *blockingBody) Read(buffer []byte) (int, error) {
	if !b.started {
		b.started = true
		close(b.readStarted)
		<-b.release
	}
	return b.reader.Read(buffer)
}

func (b *blockingBody) Close() error {
	b.closeCount++
	return nil
}

func (b *trackingBody) Close() error {
	b.closeCount++
	return nil
}

func readResponseBody(t *testing.T, response *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	response.Body = io.NopCloser(bytes.NewReader(body))
	return string(body)
}

func roundTripAsync(broker *externalgateway.Broker, ctx context.Context, slot externalgateway.SlotKey, request *http.Request) <-chan struct {
	response *http.Response
	err      error
} {
	result := make(chan struct {
		response *http.Response
		err      error
	}, 1)
	go func() {
		response, err := broker.RoundTrip(ctx, slot, request)
		result <- struct {
			response *http.Response
			err      error
		}{response: response, err: err}
	}()
	return result
}

func TestAuthenticationAndIdentityAreRequired(t *testing.T) {
	environment := newTestEnvironment(t, nil)

	unauthenticated := environment.post(t, externalgateway.ConnectPath, "Bearer invalid", externalgateway.ConnectRequest{})
	defer unauthenticated.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, unauthenticated.StatusCode)

	mismatch := environment.post(t, externalgateway.ConnectPath, otherToken, externalgateway.ConnectRequest{
		ProtocolVersion: externalgateway.ProtocolVersion,
		DeviceID:        validSlot.DeviceID,
		SlotID:          validSlot.SlotID,
		Runtime:         validSlot.Runtime,
		MaxConcurrency:  1,
		AllowedPaths:    []string{invokePath},
	})
	defer mismatch.Body.Close()
	assert.Equal(t, http.StatusForbidden, mismatch.StatusCode)

	session := environment.connect(t, validToken, validSlot, 1, []string{invokePath})
	wrongIdentity := environment.post(t, externalgateway.PollPath, otherToken, externalgateway.PollRequest{
		SessionID: session.SessionID, Generation: session.Generation,
	})
	defer wrongIdentity.Body.Close()
	assert.Equal(t, http.StatusConflict, wrongIdentity.StatusCode)
}

func TestOfflineBeforeDispatch(t *testing.T) {
	environment := newTestEnvironment(t, nil)

	response, err := environment.broker.RoundTrip(context.Background(), validSlot, newForwardRequest(http.MethodGet, invokePath, ""))
	require.Nil(t, response)
	require.ErrorIs(t, err, externalgateway.ErrOffline)
	require.NotErrorIs(t, err, externalgateway.ErrUnknownOutcome)
}

func TestSuccessfulRoundTripPreservesHTTPResponse(t *testing.T) {
	environment := newTestEnvironment(t, nil)
	session := environment.connect(t, validToken, validSlot, 1, []string{invokePath, cardPath})
	request := newForwardRequest(http.MethodPost, invokePath, `{"prompt":"hi"}`)
	body := &trackingBody{Reader: strings.NewReader(`{"prompt":"hi"}`)}
	request.Body = body
	request.Header.Set("Authorization", "Bearer temporal-user-secret")
	request.Header.Set("Cookie", "session=secret")
	request.Header.Set("Proxy-Authorization", "Bearer proxy-secret")
	request.Header.Set("Baggage", "secret=value")
	request.Header.Set("Traceparent", "00-secret")
	request.Header.Set("Connection", "keep-alive")
	request.Header.Set("X-A2A-Extensions", "streaming")
	result := roundTripAsync(environment.broker, context.Background(), validSlot, request)

	status, frame := environment.poll(t, validToken, session)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, externalgateway.FrameTypeRequest, frame.Type)
	assert.Equal(t, http.MethodPost, frame.Method)
	assert.Equal(t, invokePath, frame.Path)
	assert.JSONEq(t, `{"prompt":"hi"}`, string(frame.Body))
	assert.NotZero(t, frame.DeadlineUnixMilli)
	assert.Equal(t, 1, body.closeCount)
	assert.Equal(t, "streaming", frame.Headers.Get("X-A2A-Extensions"))
	assert.Empty(t, frame.Headers.Get("Authorization"))
	assert.Empty(t, frame.Headers.Get("Cookie"))
	assert.Empty(t, frame.Headers.Get("Proxy-Authorization"))
	assert.Empty(t, frame.Headers.Get("Baggage"))
	assert.Empty(t, frame.Headers.Get("Traceparent"))
	assert.Empty(t, frame.Headers.Get("Connection"))

	require.Equal(t, http.StatusNoContent, environment.complete(t, validToken, session, frame, http.StatusCreated, http.Header{"ETag": []string{"agent-card-v1"}, "Set-Cookie": []string{"must-not-cross=secret"}}, []byte("done")))
	outcome := <-result
	require.NoError(t, outcome.err)
	require.NotNil(t, outcome.response)
	defer outcome.response.Body.Close()
	assert.Equal(t, http.StatusCreated, outcome.response.StatusCode)
	assert.Equal(t, "agent-card-v1", outcome.response.Header.Get("ETag"))
	assert.Empty(t, outcome.response.Header.Get("Set-Cookie"))
	assert.Equal(t, "done", readResponseBody(t, outcome.response))
}

func TestDuplicateConnectionFencesOldGeneration(t *testing.T) {
	environment := newTestEnvironment(t, nil)
	first := environment.connect(t, validToken, validSlot, 1, []string{invokePath})
	second := environment.connect(t, validToken, validSlot, 1, []string{invokePath})
	require.Greater(t, second.Generation, first.Generation)
	require.NotEqual(t, second.SessionID, first.SessionID)

	stalePoll := environment.post(t, externalgateway.PollPath, validToken, externalgateway.PollRequest{SessionID: first.SessionID, Generation: first.Generation})
	defer stalePoll.Body.Close()
	assert.Equal(t, http.StatusConflict, stalePoll.StatusCode)

	status, _ := environment.poll(t, validToken, second)
	assert.Equal(t, http.StatusNoContent, status)
}

func TestDisconnectAfterDispatchReturnsUnknownOutcomeWithoutReplay(t *testing.T) {
	environment := newTestEnvironment(t, nil)
	first := environment.connect(t, validToken, validSlot, 1, []string{invokePath})
	result := roundTripAsync(environment.broker, context.Background(), validSlot, newForwardRequest(http.MethodPost, invokePath, "side-effect"))

	status, frame := environment.poll(t, validToken, first)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, externalgateway.FrameTypeRequest, frame.Type)

	second := environment.connect(t, validToken, validSlot, 1, []string{invokePath})
	outcome := <-result
	require.Nil(t, outcome.response)
	require.ErrorIs(t, outcome.err, externalgateway.ErrUnknownOutcome)
	require.NotErrorIs(t, outcome.err, externalgateway.ErrOffline)

	status, _ = environment.poll(t, validToken, second)
	assert.Equal(t, http.StatusNoContent, status, "a dispatched request must never be replayed")
}

func TestConnectionReplacementDoesNotMoveCapacityWaiterToNewGeneration(t *testing.T) {
	environment := newTestEnvironment(t, nil)
	firstSession := environment.connect(t, validToken, validSlot, 1, []string{invokePath})
	firstResult := roundTripAsync(environment.broker, context.Background(), validSlot, newForwardRequest(http.MethodPost, invokePath, "first"))
	status, _ := environment.poll(t, validToken, firstSession)
	require.Equal(t, http.StatusOK, status)

	body := &blockingBody{
		reader:      strings.NewReader("second"),
		readStarted: make(chan struct{}),
		release:     make(chan struct{}),
	}
	secondRequest := newForwardRequest(http.MethodPost, invokePath, "")
	secondRequest.Body = body
	secondResult := roundTripAsync(environment.broker, context.Background(), validSlot, secondRequest)
	<-body.readStarted

	secondSession := environment.connect(t, validToken, validSlot, 1, []string{invokePath})
	close(body.release)
	secondOutcome := <-secondResult
	require.ErrorIs(t, secondOutcome.err, externalgateway.ErrOffline)
	require.NotErrorIs(t, secondOutcome.err, externalgateway.ErrUnknownOutcome)
	assert.Equal(t, 1, body.closeCount)

	firstOutcome := <-firstResult
	require.ErrorIs(t, firstOutcome.err, externalgateway.ErrUnknownOutcome)
	status, _ = environment.poll(t, validToken, secondSession)
	assert.Equal(t, http.StatusNoContent, status, "pre-dispatch waiter must not cross the generation fence")
}

func TestHeartbeatExpiryDistinguishesDispatchedRequest(t *testing.T) {
	environment := newTestEnvironment(t, func(config *externalgateway.Config) {
		config.PollTimeout = 20 * time.Millisecond
		config.HeartbeatTimeout = 80 * time.Millisecond
	})
	session := environment.connect(t, validToken, validSlot, 1, []string{invokePath})
	result := roundTripAsync(environment.broker, context.Background(), validSlot, newForwardRequest(http.MethodPost, invokePath, "side-effect"))
	status, _ := environment.poll(t, validToken, session)
	require.Equal(t, http.StatusOK, status)

	select {
	case outcome := <-result:
		require.ErrorIs(t, outcome.err, externalgateway.ErrUnknownOutcome)
	case <-time.After(time.Second):
		t.Fatal("round trip did not observe heartbeat expiry")
	}

	_, err := environment.broker.RoundTrip(context.Background(), validSlot, newForwardRequest(http.MethodGet, invokePath, ""))
	require.ErrorIs(t, err, externalgateway.ErrOffline)
}

func TestCancellationIsDeliveredAndLateCompletionIsRejected(t *testing.T) {
	environment := newTestEnvironment(t, nil)
	session := environment.connect(t, validToken, validSlot, 1, []string{invokePath})
	ctx, cancel := context.WithCancel(context.Background())
	result := roundTripAsync(environment.broker, ctx, validSlot, newForwardRequest(http.MethodPost, invokePath, "work"))

	status, requestFrame := environment.poll(t, validToken, session)
	require.Equal(t, http.StatusOK, status)
	cancel()
	outcome := <-result
	require.ErrorIs(t, outcome.err, context.Canceled)

	status, cancelFrame := environment.poll(t, validToken, session)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, externalgateway.FrameTypeCancel, cancelFrame.Type)
	assert.Equal(t, requestFrame.RequestID, cancelFrame.RequestID)
	assert.Empty(t, cancelFrame.Method)

	lateStatus := environment.complete(t, validToken, session, requestFrame, http.StatusOK, nil, nil)
	assert.Equal(t, http.StatusConflict, lateStatus)
}

func TestBoundsAndPathAllowlist(t *testing.T) {
	environment := newTestEnvironment(t, func(config *externalgateway.Config) {
		config.MaxBodyBytes = 8
		config.MaxHeaderBytes = 64
		config.MaxHeaderCount = 2
		config.MaxConcurrencyPerSlot = 2
	})

	tooConcurrent := environment.post(t, externalgateway.ConnectPath, validToken, externalgateway.ConnectRequest{
		ProtocolVersion: 1, DeviceID: validSlot.DeviceID, SlotID: validSlot.SlotID, Runtime: validSlot.Runtime,
		MaxConcurrency: 3, AllowedPaths: []string{invokePath},
	})
	defer tooConcurrent.Body.Close()
	assert.Equal(t, http.StatusBadRequest, tooConcurrent.StatusCode)

	session := environment.connect(t, validToken, validSlot, 1, []string{invokePath})
	oversizedRequest := newForwardRequest(http.MethodPost, invokePath, "123456789")
	oversizedBody := &trackingBody{Reader: strings.NewReader("123456789")}
	oversizedRequest.Body = oversizedBody
	_, err := environment.broker.RoundTrip(context.Background(), validSlot, oversizedRequest)
	require.ErrorIs(t, err, externalgateway.ErrInvalidRequest)
	assert.Equal(t, 1, oversizedBody.closeCount)

	_, err = environment.broker.RoundTrip(context.Background(), validSlot, newForwardRequest(http.MethodGet, cardPath, ""))
	require.ErrorIs(t, err, externalgateway.ErrInvalidRequest)

	queryRequest := newForwardRequest(http.MethodGet, invokePath, "")
	queryRequest.URL.RawQuery = "token=secret"
	_, err = environment.broker.RoundTrip(context.Background(), validSlot, queryRequest)
	require.ErrorIs(t, err, externalgateway.ErrInvalidRequest)

	oversizedHeader := newForwardRequest(http.MethodGet, invokePath, "")
	oversizedHeader.Header.Set("Content-Type", strings.Repeat("x", 65))
	_, err = environment.broker.RoundTrip(context.Background(), validSlot, oversizedHeader)
	require.ErrorIs(t, err, externalgateway.ErrInvalidRequest)

	validResult := roundTripAsync(environment.broker, context.Background(), validSlot, newForwardRequest(http.MethodGet, invokePath, ""))
	status, frame := environment.poll(t, validToken, session)
	require.Equal(t, http.StatusOK, status)
	tooLargeCompletion := environment.complete(t, validToken, session, frame, http.StatusOK, nil, []byte("123456789"))
	assert.Equal(t, http.StatusRequestEntityTooLarge, tooLargeCompletion)

	require.Equal(t, http.StatusNoContent, environment.complete(t, validToken, session, frame, http.StatusOK, nil, []byte("12345678")))
	require.NoError(t, (<-validResult).err)
}

func TestTransportIDsMustBeLowercaseDNSLabels(t *testing.T) {
	environment := newTestEnvironment(t, nil)
	invalidIDs := []string{"Device-One", "device_one", "device.one", "-device", "device-"}
	for _, deviceID := range invalidIDs {
		t.Run(deviceID, func(t *testing.T) {
			response, err := environment.broker.RoundTrip(context.Background(), externalgateway.SlotKey{
				DeviceID: deviceID,
				SlotID:   validSlot.SlotID,
				Runtime:  validSlot.Runtime,
			}, newForwardRequest(http.MethodGet, invokePath, ""))
			require.Nil(t, response)
			require.ErrorIs(t, err, externalgateway.ErrInvalidRequest)
		})
	}
}

func TestMultiplexingHonorsMaxConcurrency(t *testing.T) {
	environment := newTestEnvironment(t, nil)
	session := environment.connect(t, validToken, validSlot, 2, []string{invokePath})

	var results []<-chan struct {
		response *http.Response
		err      error
	}
	for index := range 3 {
		request := newForwardRequest(http.MethodPost, invokePath, fmt.Sprintf("request-%d", index))
		results = append(results, roundTripAsync(environment.broker, context.Background(), validSlot, request))
	}

	status, first := environment.poll(t, validToken, session)
	require.Equal(t, http.StatusOK, status)
	status, second := environment.poll(t, validToken, session)
	require.Equal(t, http.StatusOK, status)
	require.NotEqual(t, first.RequestID, second.RequestID)

	status, _ = environment.poll(t, validToken, session)
	require.Equal(t, http.StatusNoContent, status, "third request must wait for an in-flight slot")

	require.Equal(t, http.StatusNoContent, environment.complete(t, validToken, session, first, http.StatusOK, nil, []byte("first")))
	status, third := environment.poll(t, validToken, session)
	require.Equal(t, http.StatusOK, status)
	require.NotEqual(t, first.RequestID, third.RequestID)
	require.NotEqual(t, second.RequestID, third.RequestID)

	require.Equal(t, http.StatusNoContent, environment.complete(t, validToken, session, second, http.StatusOK, nil, []byte("second")))
	require.Equal(t, http.StatusNoContent, environment.complete(t, validToken, session, third, http.StatusOK, nil, []byte("third")))

	for _, result := range results {
		outcome := <-result
		require.NoError(t, outcome.err)
		if outcome.response != nil {
			require.NoError(t, outcome.response.Body.Close())
		}
	}
}
