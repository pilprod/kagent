package externalgateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	pathpkg "path"
	"strings"
	"sync"
	"time"
)

const (
	defaultPollTimeout             = 25 * time.Second
	defaultHeartbeatTimeout        = 75 * time.Second
	defaultRequestTimeout          = 5 * time.Minute
	defaultMaxBodyBytes      int64 = 1 << 20
	defaultMaxHeaderBytes    int64 = 32 << 10
	defaultMaxHeaderCount          = 64
	defaultMaxAllowedPaths         = 16
	defaultMaxConcurrency          = 8
	maxPollTimeout                 = 60 * time.Second
	minAdvertisedPollTimeout       = time.Second
	maxGeneration            int64 = int64(^uint64(0) >> 1)
)

type result struct {
	response *http.Response
	err      error
}

type pendingRequest struct {
	id         string
	request    *http.Request
	frame      Frame
	dispatched bool
	cancelled  bool
	result     chan result
}

type connection struct {
	key            SlotKey
	sessionDigest  [sha256.Size]byte
	generation     int64
	allowedPaths   map[string]struct{}
	maxConcurrency int
	lastHeartbeat  time.Time
	queue          []*pendingRequest
	cancelQueue    []*pendingRequest
	pending        map[string]*pendingRequest
	changed        chan struct{}
}

func (c *connection) signalLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// Broker is an http.Handler for external clients and provides a keyed
// RoundTrip method for kagent callers.
type Broker struct {
	config        Config
	authenticator Authenticator

	mu          sync.Mutex
	slots       map[SlotKey]*connection
	sessions    map[[sha256.Size]byte]*connection
	generations map[SlotKey]int64
}

var _ http.Handler = (*Broker)(nil)

func NewBroker(config Config, authenticator Authenticator) (*Broker, error) {
	if authenticator == nil {
		return nil, fmt.Errorf("authenticator is required")
	}
	config = configWithDefaults(config)
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	return &Broker{
		config:        config,
		authenticator: authenticator,
		slots:         make(map[SlotKey]*connection),
		sessions:      make(map[[sha256.Size]byte]*connection),
		generations:   make(map[SlotKey]int64),
	}, nil
}

func configWithDefaults(config Config) Config {
	if config.PollTimeout == 0 {
		config.PollTimeout = defaultPollTimeout
	}
	if config.HeartbeatTimeout == 0 {
		config.HeartbeatTimeout = defaultHeartbeatTimeout
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = defaultRequestTimeout
	}
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = defaultMaxBodyBytes
	}
	if config.MaxHeaderBytes == 0 {
		config.MaxHeaderBytes = defaultMaxHeaderBytes
	}
	if config.MaxHeaderCount == 0 {
		config.MaxHeaderCount = defaultMaxHeaderCount
	}
	if config.MaxAllowedPaths == 0 {
		config.MaxAllowedPaths = defaultMaxAllowedPaths
	}
	if config.MaxConcurrencyPerSlot == 0 {
		config.MaxConcurrencyPerSlot = defaultMaxConcurrency
	}
	return config
}

func validateConfig(config Config) error {
	if config.PollTimeout <= 0 || config.PollTimeout > maxPollTimeout || config.HeartbeatTimeout <= config.PollTimeout {
		return fmt.Errorf("poll timeout must be at most 60 seconds and less than heartbeat timeout")
	}
	if config.RequestTimeout <= 0 {
		return fmt.Errorf("request timeout must be positive")
	}
	if config.MaxBodyBytes <= 0 || config.MaxHeaderBytes <= 0 || config.MaxHeaderCount <= 0 || config.MaxAllowedPaths <= 0 || config.MaxConcurrencyPerSlot <= 0 {
		return fmt.Errorf("protocol limits must be positive")
	}
	return nil
}

// RoundTrip dispatches request to the currently connected client for slot.
// ErrOffline is returned only before dispatch. Once a request frame has been
// delivered, connection loss returns ErrUnknownOutcome and this broker never
// retries the request.
func (b *Broker) RoundTrip(ctx context.Context, slot SlotKey, request *http.Request) (*http.Response, error) {
	if request == nil {
		return nil, fmt.Errorf("request and URL are required: %w", ErrInvalidRequest)
	}
	if ctx == nil || request.URL == nil {
		return nil, closeRequestBody(request, fmt.Errorf("context and URL are required: %w", ErrInvalidRequest))
	}
	if err := validateSlotKey(slot); err != nil {
		return nil, closeRequestBody(request, err)
	}

	b.mu.Lock()
	selectedConnection := b.currentConnectionLocked(slot, time.Now())
	b.mu.Unlock()
	if selectedConnection == nil {
		return nil, closeRequestBody(request, &OfflineError{Slot: slot})
	}

	frame, err := b.requestFrame(ctx, request)
	if err != nil {
		return nil, err
	}

	requestCtx, cancel := boundedContext(ctx, b.config.RequestTimeout)
	defer cancel()
	frame.DeadlineUnixMilli = deadlineUnixMilli(requestCtx)

	pending := &pendingRequest{
		id:      frame.RequestID,
		request: request,
		frame:   frame,
		result:  make(chan result, 1),
	}

	for {
		b.mu.Lock()
		connection := b.currentConnectionLocked(slot, time.Now())
		if connection == nil {
			b.mu.Unlock()
			return nil, &OfflineError{Slot: slot}
		}
		if connection != selectedConnection {
			b.mu.Unlock()
			return nil, &OfflineError{Slot: slot}
		}
		if _, allowed := connection.allowedPaths[frame.Path]; !allowed {
			b.mu.Unlock()
			return nil, fmt.Errorf("path is not allowed for external runtime slot: %w", ErrInvalidRequest)
		}
		if len(connection.pending) < connection.maxConcurrency {
			connection.pending[pending.id] = pending
			connection.queue = append(connection.queue, pending)
			connection.signalLocked()
			b.mu.Unlock()
			break
		}
		changed := connection.changed
		expiresAt := connection.lastHeartbeat.Add(b.config.HeartbeatTimeout)
		b.mu.Unlock()

		if err := waitForChange(requestCtx, changed, expiresAt); err != nil {
			if requestCtx.Err() != nil {
				return nil, requestCtx.Err()
			}
		}
	}

	return b.awaitResult(requestCtx, slot, pending)
}

func closeRequestBody(request *http.Request, returnErr error) error {
	if request.Body == nil {
		return returnErr
	}
	if err := request.Body.Close(); err != nil {
		return errors.Join(returnErr, fmt.Errorf("failed to close HTTP request body: %w", err))
	}
	return returnErr
}

func (b *Broker) awaitResult(ctx context.Context, slot SlotKey, pending *pendingRequest) (*http.Response, error) {
	for {
		select {
		case outcome := <-pending.result:
			return outcome.response, outcome.err
		default:
		}

		b.mu.Lock()
		connection := b.slots[slot]
		if connection == nil || connection.pending[pending.id] != pending {
			b.mu.Unlock()
			select {
			case outcome := <-pending.result:
				return outcome.response, outcome.err
			default:
				return nil, &OfflineError{Slot: slot}
			}
		}
		expiresAt := connection.lastHeartbeat.Add(b.config.HeartbeatTimeout)
		changed := connection.changed
		b.mu.Unlock()

		timer := time.NewTimer(max(time.Until(expiresAt), time.Millisecond))
		select {
		case outcome := <-pending.result:
			if !timer.Stop() {
				<-timer.C
			}
			return outcome.response, outcome.err
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			if !b.cancelPending(slot, pending) {
				select {
				case outcome := <-pending.result:
					return outcome.response, outcome.err
				default:
				}
			}
			return nil, ctx.Err()
		case <-changed:
			if !timer.Stop() {
				<-timer.C
			}
			continue
		case <-timer.C:
			b.mu.Lock()
			b.currentConnectionLocked(slot, time.Now())
			b.mu.Unlock()
		}
	}
}

func (b *Broker) requestFrame(ctx context.Context, request *http.Request) (frame Frame, returnErr error) {
	if request.Body != nil {
		defer func() {
			if err := request.Body.Close(); err != nil && returnErr == nil {
				returnErr = fmt.Errorf("failed to close HTTP request body: %w", err)
			}
		}()
	}
	if err := ctx.Err(); err != nil {
		return Frame{}, err
	}
	if request.Method != http.MethodGet && request.Method != http.MethodPost {
		return Frame{}, fmt.Errorf("method %q is not supported: %w", request.Method, ErrInvalidRequest)
	}
	requestPath, err := validateForwardPath(request.URL)
	if err != nil {
		return Frame{}, err
	}
	headers, err := validatedRequestHeaders(request.Header, b.config.MaxHeaderCount, b.config.MaxHeaderBytes)
	if err != nil {
		return Frame{}, err
	}
	body, err := readBounded(request.Body, b.config.MaxBodyBytes)
	if err != nil {
		return Frame{}, err
	}
	requestID, err := randomID(16)
	if err != nil {
		return Frame{}, fmt.Errorf("failed to generate request identifier: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Frame{}, err
	}
	return Frame{
		Type:      FrameTypeRequest,
		RequestID: requestID,
		Method:    request.Method,
		Path:      requestPath,
		Headers:   headers,
		Body:      body,
	}, nil
}

func (b *Broker) cancelPending(slot SlotKey, pending *pendingRequest) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	connection := b.slots[slot]
	if connection == nil || connection.pending[pending.id] != pending {
		return false
	}
	if !pending.dispatched {
		connection.queue = removePending(connection.queue, pending)
		delete(connection.pending, pending.id)
		connection.signalLocked()
		return true
	}
	if pending.cancelled {
		return true
	}
	pending.cancelled = true
	connection.cancelQueue = append(connection.cancelQueue, pending)
	connection.signalLocked()
	return true
}

func (b *Broker) currentConnectionLocked(slot SlotKey, now time.Time) *connection {
	connection := b.slots[slot]
	if connection == nil {
		return nil
	}
	if now.Sub(connection.lastHeartbeat) <= b.config.HeartbeatTimeout {
		return connection
	}
	b.closeConnectionLocked(connection)
	return nil
}

func (b *Broker) closeConnectionLocked(connection *connection) {
	if b.slots[connection.key] == connection {
		delete(b.slots, connection.key)
	}
	delete(b.sessions, connection.sessionDigest)
	for _, pending := range connection.pending {
		var err error = &OfflineError{Slot: connection.key}
		if pending.dispatched {
			err = &UnknownOutcomeError{Slot: connection.key}
		}
		select {
		case pending.result <- result{err: err}:
		default:
		}
	}
	connection.pending = make(map[string]*pendingRequest)
	connection.queue = nil
	connection.cancelQueue = nil
	connection.signalLocked()
}

func (b *Broker) requestConnectionLocked(sessionID string, generation int64, claims Claims, now time.Time) (*connection, error) {
	digest, ok := sessionDigest(sessionID)
	if !ok {
		return nil, fmt.Errorf("invalid session")
	}
	connection := b.sessions[digest]
	if connection == nil || connection.generation != generation || b.slots[connection.key] != connection {
		return nil, fmt.Errorf("stale session")
	}
	if !claimsAllowSlot(claims, connection.key) {
		return nil, fmt.Errorf("authenticated identity does not own session")
	}
	if now.Sub(connection.lastHeartbeat) > b.config.HeartbeatTimeout {
		b.closeConnectionLocked(connection)
		return nil, fmt.Errorf("expired session")
	}
	return connection, nil
}

func removePending(queue []*pendingRequest, target *pendingRequest) []*pendingRequest {
	for index, pending := range queue {
		if pending == target {
			copy(queue[index:], queue[index+1:])
			queue[len(queue)-1] = nil
			return queue[:len(queue)-1]
		}
	}
	return queue
}

func waitForChange(ctx context.Context, changed <-chan struct{}, deadline time.Time) error {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-changed:
		return nil
	case <-timer.C:
		return nil
	}
}

func boundedContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(timeout)
	if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, deadline)
}

func deadlineUnixMilli(ctx context.Context) int64 {
	deadline, _ := ctx.Deadline()
	return deadline.UnixMilli()
}

func validateSlotKey(slot SlotKey) error {
	if !validTransportID(slot.DeviceID) || !validTransportID(slot.SlotID) {
		return fmt.Errorf("device_id and slot_id must be lowercase DNS labels: %w", ErrInvalidRequest)
	}
	if slot.Runtime != RuntimeCodex && slot.Runtime != RuntimeClaude {
		return fmt.Errorf("runtime %q is not supported: %w", slot.Runtime, ErrInvalidRequest)
	}
	return nil
}

func validTransportID(value string) bool {
	if len(value) == 0 || len(value) > 63 || !isLowerAlphaNumeric(value[0]) || !isLowerAlphaNumeric(value[len(value)-1]) {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if isLowerAlphaNumeric(character) || character == '-' {
			continue
		}
		return false
	}
	return true
}

func isLowerAlphaNumeric(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= '0' && value <= '9')
}

func validateForwardPath(value *url.URL) (string, error) {
	if value == nil || value.IsAbs() || value.Host != "" || value.User != nil || value.RawQuery != "" || value.ForceQuery || value.Fragment != "" || value.RawFragment != "" || value.Opaque != "" {
		return "", fmt.Errorf("request URL must contain only an absolute path: %w", ErrInvalidRequest)
	}
	if err := validateAllowedPath(value.Path); err != nil {
		return "", err
	}
	if value.RawPath != "" && value.RawPath != value.Path {
		return "", fmt.Errorf("escaped paths are not supported: %w", ErrInvalidRequest)
	}
	return value.Path, nil
}

func validateAllowedPath(value string) error {
	if value == "" || len(value) > 1024 || value[0] != '/' || strings.Contains(value, "\\") || strings.Contains(value, "%") || pathpkg.Clean(value) != value || strings.HasPrefix(value, "//") {
		return fmt.Errorf("path %q is not a canonical absolute path: %w", value, ErrInvalidRequest)
	}
	return nil
}

func validatedRequestHeaders(headers http.Header, maxCount int, maxBytes int64) (http.Header, error) {
	return validatedHeaders(headers, maxCount, maxBytes, requestHeaderAllowed)
}

func validatedResponseHeaders(headers http.Header, maxCount int, maxBytes int64) (http.Header, error) {
	return validatedHeaders(headers, maxCount, maxBytes, responseHeaderAllowed)
}

func validatedHeaders(headers http.Header, maxCount int, maxBytes int64, allowed func(string) bool) (http.Header, error) {
	allowedCount := 0
	for name := range headers {
		if allowed(http.CanonicalHeaderKey(name)) {
			allowedCount++
		}
	}
	if allowedCount > maxCount {
		return nil, fmt.Errorf("header count exceeds limit: %w", ErrInvalidRequest)
	}
	validated := make(http.Header, allowedCount)
	var size int64
	for name, values := range headers {
		canonicalName := http.CanonicalHeaderKey(name)
		if !allowed(canonicalName) {
			continue
		}
		if !validHeaderName(name) {
			return nil, fmt.Errorf("header name is invalid: %w", ErrInvalidRequest)
		}
		size += int64(len(name))
		for _, value := range values {
			if strings.ContainsAny(value, "\r\n\x00") {
				return nil, fmt.Errorf("header value is invalid: %w", ErrInvalidRequest)
			}
			size += int64(len(value))
		}
		if size > maxBytes {
			return nil, fmt.Errorf("header bytes exceed limit: %w", ErrInvalidRequest)
		}
		validated[canonicalName] = append([]string(nil), values...)
	}
	return validated, nil
}

func requestHeaderAllowed(name string) bool {
	switch name {
	case "Accept", "Content-Type", "X-A2a-Extensions":
		return true
	default:
		return false
	}
}

func responseHeaderAllowed(name string) bool {
	switch name {
	case "Cache-Control", "Content-Type", "Etag", "Last-Modified", "X-A2a-Extensions":
		return true
	default:
		return false
	}
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(character)) {
			continue
		}
		return false
	}
	return true
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	if reader == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read HTTP body: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("body exceeds limit: %w", ErrInvalidRequest)
	}
	return body, nil
}

func randomID(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func sessionDigest(sessionID string) ([sha256.Size]byte, bool) {
	if len(sessionID) != base64.RawURLEncoding.EncodedLen(32) {
		return [sha256.Size]byte{}, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(sessionID)
	if err != nil || len(decoded) != 32 {
		return [sha256.Size]byte{}, false
	}
	return sha256.Sum256([]byte(sessionID)), true
}

func claimsAllowSlot(claims Claims, slot SlotKey) bool {
	if claims.Subject == "" {
		return false
	}
	allowed := 0
	for _, candidate := range claims.AllowedSlots {
		allowed |= constantTimeSlotEqual(candidate, slot)
	}
	return allowed == 1
}

func constantTimeSlotEqual(left SlotKey, right SlotKey) int {
	return constantTimeStringEqual(left.DeviceID, right.DeviceID) & constantTimeStringEqual(left.SlotID, right.SlotID) & constantTimeStringEqual(string(left.Runtime), string(right.Runtime))
}

func constantTimeStringEqual(left string, right string) int {
	maxLength := max(len(left), len(right))
	if maxLength > 63 {
		return 0
	}
	leftBuffer := make([]byte, maxLength)
	rightBuffer := make([]byte, maxLength)
	copy(leftBuffer, left)
	copy(rightBuffer, right)
	equal := subtle.ConstantTimeCompare(leftBuffer, rightBuffer)
	return equal & subtle.ConstantTimeEq(int32(len(left)), int32(len(right)))
}

func newHTTPResponse(request *http.Request, completion CompleteRequest) *http.Response {
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", completion.StatusCode, http.StatusText(completion.StatusCode)),
		StatusCode:    completion.StatusCode,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        completion.Headers,
		Body:          io.NopCloser(bytes.NewReader(completion.Body)),
		ContentLength: int64(len(completion.Body)),
		Request:       request,
	}
}
