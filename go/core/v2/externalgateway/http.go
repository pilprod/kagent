package externalgateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

type errorResponse struct {
	Error string `json:"error"`
}

func (b *Broker) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.User != nil || request.URL.Fragment != "" {
		writeError(writer, http.StatusBadRequest, "query and user information are not accepted")
		return
	}
	claims, err := b.authenticator.Authenticate(request.Context(), request.Header.Clone())
	if err != nil || claims.Subject == "" {
		writeError(writer, http.StatusUnauthorized, "unauthorized")
		return
	}

	switch request.URL.Path {
	case ConnectPath:
		b.handleConnect(writer, request, claims)
	case PollPath:
		b.handlePoll(writer, request, claims)
	case CompletePath:
		b.handleComplete(writer, request, claims)
	default:
		writeError(writer, http.StatusNotFound, "not found")
	}
}

func (b *Broker) handleConnect(writer http.ResponseWriter, request *http.Request, claims Claims) {
	var input ConnectRequest
	maxWireBytes := int64(b.config.MaxAllowedPaths*1024 + 4096)
	if err := decodeJSON(writer, request, maxWireBytes, &input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid request")
		return
	}
	key := SlotKey{DeviceID: input.DeviceID, SlotID: input.SlotID, Runtime: input.Runtime}
	if input.ProtocolVersion != ProtocolVersion || input.MaxConcurrency <= 0 || input.MaxConcurrency > b.config.MaxConcurrencyPerSlot || len(input.AllowedPaths) == 0 || len(input.AllowedPaths) > b.config.MaxAllowedPaths {
		writeError(writer, http.StatusBadRequest, "invalid request")
		return
	}
	if err := validateSlotKey(key); err != nil || !claimsAllowSlot(claims, key) {
		writeError(writer, http.StatusForbidden, "identity is not authorized for slot")
		return
	}
	allowedPaths := make(map[string]struct{}, len(input.AllowedPaths))
	for _, allowedPath := range input.AllowedPaths {
		if err := validateAllowedPath(allowedPath); err != nil {
			writeError(writer, http.StatusBadRequest, "invalid request")
			return
		}
		allowedPaths[allowedPath] = struct{}{}
	}

	sessionID, err := randomID(32)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to create session")
		return
	}
	digest, ok := sessionDigest(sessionID)
	if !ok {
		writeError(writer, http.StatusInternalServerError, "failed to create session")
		return
	}

	now := time.Now()
	b.mu.Lock()
	if b.generations[key] == maxGeneration {
		b.mu.Unlock()
		writeError(writer, http.StatusServiceUnavailable, "slot generation is exhausted")
		return
	}
	if previous := b.slots[key]; previous != nil {
		b.closeConnectionLocked(previous)
	}
	generation := b.generations[key] + 1
	b.generations[key] = generation
	connection := &connection{
		key:            key,
		sessionDigest:  digest,
		generation:     generation,
		allowedPaths:   allowedPaths,
		maxConcurrency: input.MaxConcurrency,
		lastHeartbeat:  now,
		pending:        make(map[string]*pendingRequest),
		changed:        make(chan struct{}),
	}
	b.slots[key] = connection
	b.sessions[digest] = connection
	b.mu.Unlock()

	writeJSON(writer, http.StatusOK, ConnectResponse{
		SessionID:     sessionID,
		Generation:    generation,
		PollTimeoutMS: max(b.config.PollTimeout.Milliseconds(), minAdvertisedPollTimeout.Milliseconds()),
	})
}

func (b *Broker) handlePoll(writer http.ResponseWriter, request *http.Request, claims Claims) {
	var input PollRequest
	if err := decodeJSON(writer, request, 4096, &input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid request")
		return
	}

	deadline := time.NewTimer(b.config.PollTimeout)
	defer deadline.Stop()
	for {
		b.mu.Lock()
		connection, err := b.requestConnectionLocked(input.SessionID, input.Generation, claims, time.Now())
		if err != nil {
			b.mu.Unlock()
			writeError(writer, http.StatusConflict, "stale or invalid session")
			return
		}
		connection.lastHeartbeat = time.Now()
		if len(connection.cancelQueue) > 0 {
			pending := connection.cancelQueue[0]
			connection.cancelQueue = connection.cancelQueue[1:]
			delete(connection.pending, pending.id)
			connection.signalLocked()
			b.mu.Unlock()
			writeJSON(writer, http.StatusOK, Frame{Type: FrameTypeCancel, RequestID: pending.id})
			return
		}
		if len(connection.queue) > 0 {
			pending := connection.queue[0]
			connection.queue = connection.queue[1:]
			pending.dispatched = true
			connection.signalLocked()
			b.mu.Unlock()
			writeJSON(writer, http.StatusOK, pending.frame)
			return
		}
		changed := connection.changed
		b.mu.Unlock()

		select {
		case <-request.Context().Done():
			return
		case <-changed:
			continue
		case <-deadline.C:
			writer.WriteHeader(http.StatusNoContent)
			return
		}
	}
}

func (b *Broker) handleComplete(writer http.ResponseWriter, request *http.Request, claims Claims) {
	var input CompleteRequest
	maxWireBytes := b.config.MaxBodyBytes*2 + b.config.MaxHeaderBytes*2 + 4096
	if err := decodeJSON(writer, request, maxWireBytes, &input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid request")
		return
	}
	if input.StatusCode < 100 || input.StatusCode > 599 {
		writeError(writer, http.StatusBadRequest, "invalid response status")
		return
	}
	if int64(len(input.Body)) > b.config.MaxBodyBytes {
		writeError(writer, http.StatusRequestEntityTooLarge, "response body exceeds limit")
		return
	}
	headers, err := validatedResponseHeaders(input.Headers, b.config.MaxHeaderCount, b.config.MaxHeaderBytes)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid response headers")
		return
	}
	input.Headers = headers

	b.mu.Lock()
	connection, err := b.requestConnectionLocked(input.SessionID, input.Generation, claims, time.Now())
	if err != nil {
		b.mu.Unlock()
		writeError(writer, http.StatusConflict, "stale or invalid session")
		return
	}
	connection.lastHeartbeat = time.Now()
	pending := connection.pending[input.RequestID]
	if pending == nil || !pending.dispatched || pending.cancelled {
		b.mu.Unlock()
		writeError(writer, http.StatusConflict, "request is not awaiting completion")
		return
	}
	delete(connection.pending, input.RequestID)
	connection.signalLocked()
	response := newHTTPResponse(pending.request, input)
	select {
	case pending.result <- result{response: response}:
	default:
		b.mu.Unlock()
		writeError(writer, http.StatusConflict, "request is not awaiting completion")
		return
	}
	b.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, limit int64, target any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, limit)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("request contains trailing JSON")
	}
	return nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	payload, err := json.Marshal(value)
	if err != nil {
		http.Error(writer, "failed to encode response", http.StatusInternalServerError)
		return
	}
	payload = append(payload, '\n')
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if _, err := writer.Write(payload); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, errorResponse{Error: message})
}
