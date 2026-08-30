package externalgateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const (
	// ProtocolVersion is the only reverse HTTPS wire version supported here.
	ProtocolVersion = 1

	// ConnectPath registers and fences one runtime slot session.
	ConnectPath = "/v1/connect"
	// PollPath long-polls for request and cancellation frames.
	PollPath = "/v1/poll"
	// CompletePath accepts the response to one dispatched request.
	CompletePath = "/v1/complete"
)

var (
	// ErrOffline means the request was not dispatched to an external runtime.
	// It is safe for a higher layer to apply its normal pre-dispatch retry policy.
	ErrOffline = errors.New("external runtime slot is offline")
	// ErrUnknownOutcome means the request was dispatched before the connection
	// was lost. Retrying it can duplicate side effects and is therefore unsafe.
	ErrUnknownOutcome = errors.New("external runtime request outcome is unknown")
	// ErrInvalidRequest means a request cannot be represented by protocol v1 or
	// violates the configured transport bounds.
	ErrInvalidRequest = errors.New("invalid external runtime request")
)

// Runtime identifies the supported local agent implementation.
type Runtime string

const (
	// RuntimeCodex identifies a Codex runtime slot.
	RuntimeCodex Runtime = "codex"
	// RuntimeClaude identifies a Claude Code runtime slot.
	RuntimeClaude Runtime = "claude"
)

// SlotKey is the stable identity used by kagent to select an external runtime.
type SlotKey struct {
	DeviceID string
	SlotID   string
	Runtime  Runtime
}

// Claims are the authenticated identity and slot grants associated with one
// HTTP request. Subject is used only to require a non-anonymous principal and
// is never logged by this package.
type Claims struct {
	Subject      string
	AllowedSlots []SlotKey
}

// Authenticator validates request credentials and returns their slot grants.
// Implementations must not retain or log credential-bearing headers.
type Authenticator interface {
	Authenticate(ctx context.Context, headers http.Header) (Claims, error)
}

// Config contains hard protocol and resource limits. Zero values are replaced
// by conservative defaults in NewBroker.
type Config struct {
	PollTimeout           time.Duration
	HeartbeatTimeout      time.Duration
	RequestTimeout        time.Duration
	MaxBodyBytes          int64
	MaxHeaderBytes        int64
	MaxHeaderCount        int
	MaxAllowedPaths       int
	MaxConcurrencyPerSlot int
}

// OfflineError identifies the slot that was offline without exposing a
// credential or session identifier.
type OfflineError struct {
	Slot SlotKey
}

func (e *OfflineError) Error() string {
	return fmt.Sprintf("external runtime slot %s/%s/%s is offline", e.Slot.DeviceID, e.Slot.SlotID, e.Slot.Runtime)
}

func (e *OfflineError) Unwrap() error { return ErrOffline }

// UnknownOutcomeError identifies the slot whose dispatched request lost its
// connection. The opaque request identifier is intentionally omitted.
type UnknownOutcomeError struct {
	Slot SlotKey
}

func (e *UnknownOutcomeError) Error() string {
	return fmt.Sprintf("external runtime request for slot %s/%s/%s has unknown outcome", e.Slot.DeviceID, e.Slot.SlotID, e.Slot.Runtime)
}

func (e *UnknownOutcomeError) Unwrap() error { return ErrUnknownOutcome }

// ConnectRequest registers one authenticated local runtime slot.
type ConnectRequest struct {
	ProtocolVersion int      `json:"protocol_version"`
	DeviceID        string   `json:"device_id"`
	SlotID          string   `json:"slot_id"`
	Runtime         Runtime  `json:"runtime"`
	MaxConcurrency  int      `json:"max_concurrency"`
	AllowedPaths    []string `json:"allowed_paths"`
}

// ConnectResponse contains the opaque fenced session coordinates.
type ConnectResponse struct {
	SessionID     string `json:"session_id"`
	Generation    int64  `json:"generation"`
	PollTimeoutMS int64  `json:"poll_timeout_ms"`
}

// PollRequest waits for the next request or cancellation frame.
type PollRequest struct {
	SessionID  string `json:"session_id"`
	Generation int64  `json:"generation"`
}

// FrameType discriminates reverse-channel frames.
type FrameType string

const (
	// FrameTypeRequest carries a bounded local HTTP request.
	FrameTypeRequest FrameType = "request"
	// FrameTypeCancel cancels a previously dispatched request.
	FrameTypeCancel FrameType = "cancel"
)

// Frame is either a request (all fields) or a cancellation (type and
// request_id only). Body uses encoding/json's standard base64 representation.
type Frame struct {
	Type              FrameType   `json:"type"`
	RequestID         string      `json:"request_id"`
	Method            string      `json:"method,omitempty"`
	Path              string      `json:"path,omitempty"`
	Headers           http.Header `json:"headers,omitempty"`
	Body              []byte      `json:"body,omitempty"`
	DeadlineUnixMilli int64       `json:"deadline_unix_milli,omitempty"`
}

// CompleteRequest returns the exact bounded response for a dispatched request.
type CompleteRequest struct {
	SessionID  string      `json:"session_id"`
	Generation int64       `json:"generation"`
	RequestID  string      `json:"request_id"`
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	Body       []byte      `json:"body"`
}
