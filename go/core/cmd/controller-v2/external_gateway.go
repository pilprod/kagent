package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	"github.com/kagent-dev/kagent/go/core/v2/externalgateway"
)

const (
	defaultExternalGatewayBindAddress = ":8085"
	defaultExternalProbeTimeout       = 30 * time.Second
	externalGatewayShutdownTimeout    = 70 * time.Second
	maxDeviceTokenBytes               = 4096
)

var errDeviceAuthentication = errors.New("external gateway device authentication failed")

// externalGatewayConfig is intentionally process-local. The broker keeps its
// sessions in memory, so the controller-v2 Deployment must remain at one
// replica until session ownership is externalized.
type externalGatewayConfig struct {
	Enabled      bool
	BindAddress  string
	TokenFile    string
	DeviceID     string
	placements   map[dbpkg.ExternalRuntime]externalgateway.SlotKey
	ProbeTimeout time.Duration
	Broker       externalgateway.Config
}

func loadExternalGatewayConfig(getenv func(string) string) (externalGatewayConfig, error) {
	if getenv == nil {
		return externalGatewayConfig{}, fmt.Errorf("external gateway environment lookup is nil")
	}

	enabled, err := boolSetting(getenv, "EXTERNAL_GATEWAY_ENABLED", false)
	if err != nil {
		return externalGatewayConfig{}, err
	}
	if !enabled {
		return externalGatewayConfig{}, nil
	}

	config := externalGatewayConfig{
		Enabled:     true,
		BindAddress: valueOrDefault(getenv("EXTERNAL_GATEWAY_BIND_ADDRESS"), defaultExternalGatewayBindAddress),
		TokenFile:   getenv("EXTERNAL_GATEWAY_TOKEN_FILE"),
		DeviceID:    getenv("EXTERNAL_GATEWAY_DEVICE_ID"),
		placements:  make(map[dbpkg.ExternalRuntime]externalgateway.SlotKey, 2),
	}
	if config.TokenFile == "" {
		return externalGatewayConfig{}, fmt.Errorf("EXTERNAL_GATEWAY_TOKEN_FILE is required")
	}

	if config.DeviceID == "" {
		return externalGatewayConfig{}, fmt.Errorf("EXTERNAL_GATEWAY_DEVICE_ID is required")
	}
	if slotID := getenv("EXTERNAL_GATEWAY_CODEX_SLOT_ID"); slotID != "" {
		config.placements[dbpkg.ExternalRuntimeCodex] = externalgateway.SlotKey{DeviceID: config.DeviceID, SlotID: slotID, Runtime: externalgateway.RuntimeCodex}
	}
	if slotID := getenv("EXTERNAL_GATEWAY_CLAUDE_SLOT_ID"); slotID != "" {
		config.placements[dbpkg.ExternalRuntimeClaude] = externalgateway.SlotKey{DeviceID: config.DeviceID, SlotID: slotID, Runtime: externalgateway.RuntimeClaude}
	}
	if len(config.placements) == 0 {
		return externalGatewayConfig{}, fmt.Errorf("external gateway requires at least one complete Codex or Claude placement")
	}

	config.ProbeTimeout, err = durationSetting(getenv, "EXTERNAL_GATEWAY_PROBE_TIMEOUT", defaultExternalProbeTimeout)
	if err != nil {
		return externalGatewayConfig{}, err
	}
	config.Broker.PollTimeout, err = durationSetting(getenv, "EXTERNAL_GATEWAY_POLL_TIMEOUT", 25*time.Second)
	if err != nil {
		return externalGatewayConfig{}, err
	}
	config.Broker.HeartbeatTimeout, err = durationSetting(getenv, "EXTERNAL_GATEWAY_HEARTBEAT_TIMEOUT", 75*time.Second)
	if err != nil {
		return externalGatewayConfig{}, err
	}
	config.Broker.RequestTimeout, err = durationSetting(getenv, "EXTERNAL_GATEWAY_REQUEST_TIMEOUT", 5*time.Minute)
	if err != nil {
		return externalGatewayConfig{}, err
	}
	config.Broker.MaxConcurrencyPerSlot, err = positiveIntSetting(getenv, "EXTERNAL_GATEWAY_MAX_CONCURRENCY_PER_SLOT", 8)
	if err != nil {
		return externalGatewayConfig{}, err
	}
	return config, nil
}

func (c externalGatewayConfig) slots() []externalgateway.SlotKey {
	result := make([]externalgateway.SlotKey, 0, len(c.placements))
	if slot, exists := c.placements[dbpkg.ExternalRuntimeCodex]; exists {
		result = append(result, slot)
	}
	if slot, exists := c.placements[dbpkg.ExternalRuntimeClaude]; exists {
		result = append(result, slot)
	}
	return result
}

func (c externalGatewayConfig) placement() map[dbpkg.ExternalRuntime]externalgateway.SlotKey {
	result := make(map[dbpkg.ExternalRuntime]externalgateway.SlotKey, len(c.placements))
	for runtime, slot := range c.placements {
		result[runtime] = slot
	}
	return result
}

func boolSetting(getenv func(string) string, name string, fallback bool) (bool, error) {
	raw := getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return value, nil
}

func durationSetting(getenv func(string) string, name string, fallback time.Duration) (time.Duration, error) {
	raw := getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return value, nil
}

func positiveIntSetting(getenv func(string) string, name string, fallback int) (int, error) {
	raw := getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// deviceTokenAuthenticator recognizes only the reverse connector's dedicated
// Bearer token. User and Temporal credentials are unrelated and fail closed.
// Only a fixed-length digest is retained after startup.
type deviceTokenAuthenticator struct {
	tokenDigest [sha256.Size]byte
	subject     string
	slots       []externalgateway.SlotKey
}

var _ externalgateway.Authenticator = (*deviceTokenAuthenticator)(nil)

func newDeviceTokenAuthenticatorFromFile(path, subject string, slots []externalgateway.SlotKey) (*deviceTokenAuthenticator, error) {
	if path == "" {
		return nil, fmt.Errorf("external gateway token file is required")
	}
	token, err := readDeviceToken(path)
	if err != nil {
		return nil, err
	}
	defer clear(token)

	// Kubernetes Secret volumes do not append a newline, while operator-created
	// files commonly do. Accept exactly one terminal LF or CRLF, never arbitrary
	// surrounding whitespace that could hide a configuration error.
	token = bytes.TrimSuffix(token, []byte("\n"))
	token = bytes.TrimSuffix(token, []byte("\r"))
	if err := validateDeviceToken(token); err != nil {
		return nil, err
	}
	if subject == "" || len(slots) == 0 {
		return nil, fmt.Errorf("external gateway device identity and slots are required")
	}
	for _, slot := range slots {
		if slot.DeviceID != subject {
			return nil, fmt.Errorf("external gateway token grants must belong to one device identity")
		}
	}

	return &deviceTokenAuthenticator{
		tokenDigest: sha256.Sum256(token),
		subject:     subject,
		slots:       append([]externalgateway.SlotKey(nil), slots...),
	}, nil
}

func readDeviceToken(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open external gateway token file: %w", err)
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() {
		closeErr := file.Close()
		if statErr == nil {
			statErr = fmt.Errorf("external gateway token file is not a regular file")
		}
		return nil, errors.Join(statErr, closeErr)
	}
	token, readErr := io.ReadAll(io.LimitReader(file, maxDeviceTokenBytes+3))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		clear(token)
		return nil, fmt.Errorf("read external gateway token file: %w", errors.Join(readErr, closeErr))
	}
	return token, nil
}

func validateDeviceToken(token []byte) error {
	if len(token) < 32 || len(token) > maxDeviceTokenBytes {
		return fmt.Errorf("external gateway token must contain between 32 and %d bytes", maxDeviceTokenBytes)
	}
	for _, character := range token {
		if character < 0x21 || character > 0x7e {
			return fmt.Errorf("external gateway token must contain visible ASCII characters only")
		}
	}
	return nil
}

func (a *deviceTokenAuthenticator) Authenticate(_ context.Context, headers http.Header) (externalgateway.Claims, error) {
	presented, wellFormed := deviceBearerToken(headers)
	presentedDigest := sha256.Sum256([]byte(presented))
	digestMatches := subtle.ConstantTimeCompare(a.tokenDigest[:], presentedDigest[:])
	if !wellFormed || digestMatches != 1 {
		return externalgateway.Claims{}, errDeviceAuthentication
	}
	return externalgateway.Claims{
		Subject:      a.subject,
		AllowedSlots: append([]externalgateway.SlotKey(nil), a.slots...),
	}, nil
}

func deviceBearerToken(headers http.Header) (string, bool) {
	values := headers.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	fields := strings.Fields(values[0])
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || fields[1] == "" {
		return "", false
	}
	return fields[1], true
}

func newExternalGatewayHTTPServer(address string, handler http.Handler) (*http.Server, error) {
	if address == "" {
		return nil, fmt.Errorf("external gateway bind address is required")
	}
	if handler == nil {
		return nil, fmt.Errorf("external gateway handler is required")
	}
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      65 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}, nil
}

func serveHTTP(ctx context.Context, server *http.Server, listener net.Listener) error {
	if ctx == nil || server == nil || listener == nil {
		return fmt.Errorf("HTTP server, listener, and context are required")
	}

	serveDone := make(chan struct{})
	shutdownDone := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), externalGatewayShutdownTimeout)
			defer cancel()
			shutdownDone <- server.Shutdown(shutdownCtx)
		case <-serveDone:
			shutdownDone <- nil
		}
	}()

	serveErr := server.Serve(listener)
	close(serveDone)
	shutdownErr := <-shutdownDone
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, shutdownErr)
}
