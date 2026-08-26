package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	"github.com/kagent-dev/kagent/go/core/v2/externalgateway"
)

func TestLoadExternalGatewayConfigDisabledDoesNotReadDeviceSettings(t *testing.T) {
	var reads []string
	config, err := loadExternalGatewayConfig(func(name string) string {
		reads = append(reads, name)
		if name != "EXTERNAL_GATEWAY_ENABLED" {
			t.Fatalf("disabled gateway unexpectedly read %s", name)
		}
		return "false"
	})
	if err != nil {
		t.Fatalf("load disabled config: %v", err)
	}
	if config.Enabled {
		t.Fatal("gateway unexpectedly enabled")
	}
	if !reflect.DeepEqual(reads, []string{"EXTERNAL_GATEWAY_ENABLED"}) {
		t.Fatalf("unexpected settings read: %v", reads)
	}
}

func TestLoadExternalGatewayConfigRequiresCompleteExplicitPlacement(t *testing.T) {
	tests := map[string]map[string]string{
		"missing token": {
			"EXTERNAL_GATEWAY_ENABLED": "true",
		},
		"missing placement": {
			"EXTERNAL_GATEWAY_ENABLED":    "true",
			"EXTERNAL_GATEWAY_TOKEN_FILE": "/run/device-token",
			"EXTERNAL_GATEWAY_DEVICE_ID":  "device-1",
		},
		"missing device identity": {
			"EXTERNAL_GATEWAY_ENABLED":        "true",
			"EXTERNAL_GATEWAY_TOKEN_FILE":     "/run/device-token",
			"EXTERNAL_GATEWAY_CLAUDE_SLOT_ID": "claude-1",
		},
		"invalid feature gate": {
			"EXTERNAL_GATEWAY_ENABLED": "sometimes",
		},
	}
	for name, settings := range tests {
		t.Run(name, func(t *testing.T) {
			config, err := loadExternalGatewayConfig(mapEnvironment(settings))
			if err == nil || config.Enabled {
				t.Fatalf("expected fail-closed configuration error, got config=%+v err=%v", config, err)
			}
		})
	}
}

func TestLoadExternalGatewayConfigAllowsOnlyExplicitRuntimes(t *testing.T) {
	tests := []struct {
		name     string
		settings map[string]string
		want     map[dbpkg.ExternalRuntime]externalgateway.SlotKey
	}{
		{
			name: "codex only",
			settings: map[string]string{
				"EXTERNAL_GATEWAY_ENABLED":       "true",
				"EXTERNAL_GATEWAY_TOKEN_FILE":    "/run/device-token",
				"EXTERNAL_GATEWAY_DEVICE_ID":     "device-codex",
				"EXTERNAL_GATEWAY_CODEX_SLOT_ID": "slot-codex",
			},
			want: map[dbpkg.ExternalRuntime]externalgateway.SlotKey{
				dbpkg.ExternalRuntimeCodex: {DeviceID: "device-codex", SlotID: "slot-codex", Runtime: externalgateway.RuntimeCodex},
			},
		},
		{
			name: "claude only",
			settings: map[string]string{
				"EXTERNAL_GATEWAY_ENABLED":        "true",
				"EXTERNAL_GATEWAY_TOKEN_FILE":     "/run/device-token",
				"EXTERNAL_GATEWAY_DEVICE_ID":      "device-claude",
				"EXTERNAL_GATEWAY_CLAUDE_SLOT_ID": "slot-claude",
			},
			want: map[dbpkg.ExternalRuntime]externalgateway.SlotKey{
				dbpkg.ExternalRuntimeClaude: {DeviceID: "device-claude", SlotID: "slot-claude", Runtime: externalgateway.RuntimeClaude},
			},
		},
		{
			name: "both runtimes on one authenticated device",
			settings: map[string]string{
				"EXTERNAL_GATEWAY_ENABLED":                  "true",
				"EXTERNAL_GATEWAY_TOKEN_FILE":               "/run/device-token",
				"EXTERNAL_GATEWAY_DEVICE_ID":                "device-1",
				"EXTERNAL_GATEWAY_CODEX_SLOT_ID":            "slot-codex",
				"EXTERNAL_GATEWAY_CLAUDE_SLOT_ID":           "slot-claude",
				"EXTERNAL_GATEWAY_BIND_ADDRESS":             ":18085",
				"EXTERNAL_GATEWAY_PROBE_TIMEOUT":            "12s",
				"EXTERNAL_GATEWAY_POLL_TIMEOUT":             "3s",
				"EXTERNAL_GATEWAY_HEARTBEAT_TIMEOUT":        "9s",
				"EXTERNAL_GATEWAY_REQUEST_TIMEOUT":          "2m",
				"EXTERNAL_GATEWAY_MAX_CONCURRENCY_PER_SLOT": "4",
			},
			want: map[dbpkg.ExternalRuntime]externalgateway.SlotKey{
				dbpkg.ExternalRuntimeCodex:  {DeviceID: "device-1", SlotID: "slot-codex", Runtime: externalgateway.RuntimeCodex},
				dbpkg.ExternalRuntimeClaude: {DeviceID: "device-1", SlotID: "slot-claude", Runtime: externalgateway.RuntimeClaude},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, err := loadExternalGatewayConfig(mapEnvironment(test.settings))
			if err != nil {
				t.Fatalf("load enabled config: %v", err)
			}
			if !config.Enabled || !reflect.DeepEqual(config.placement(), test.want) {
				t.Fatalf("unexpected placement: enabled=%v got=%v want=%v", config.Enabled, config.placement(), test.want)
			}
			if config.BindAddress == "" || config.TokenFile != "/run/device-token" {
				t.Fatalf("unexpected listener/token-file config: %+v", config)
			}
			if test.name == "both runtimes on one authenticated device" {
				if config.BindAddress != ":18085" || config.ProbeTimeout != 12*time.Second || config.Broker.PollTimeout != 3*time.Second || config.Broker.HeartbeatTimeout != 9*time.Second || config.Broker.RequestTimeout != 2*time.Minute || config.Broker.MaxConcurrencyPerSlot != 4 {
					t.Fatalf("custom settings not applied: %+v", config)
				}
			}
		})
	}
}

func TestDeviceTokenAuthenticatorRejectsUnrelatedCredentialsWithoutLeak(t *testing.T) {
	const token = "connector-token-0123456789abcdef-0123456789abcdef"
	slot := externalgateway.SlotKey{DeviceID: "device-1", SlotID: "codex-1", Runtime: externalgateway.RuntimeCodex}
	authenticator := writeDeviceAuthenticator(t, token, []externalgateway.SlotKey{slot})

	valid := http.Header{"Authorization": []string{"Bearer " + token}}
	claims, err := authenticator.Authenticate(t.Context(), valid)
	if err != nil {
		t.Fatalf("authenticate valid device: %v", err)
	}
	if claims.Subject != slot.DeviceID || !reflect.DeepEqual(claims.AllowedSlots, []externalgateway.SlotKey{slot}) {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	claims.AllowedSlots[0].DeviceID = "mutated"
	claims, err = authenticator.Authenticate(t.Context(), valid)
	if err != nil || claims.AllowedSlots[0] != slot {
		t.Fatal("returned claims mutated authenticator grants")
	}

	unrelated := strings.Repeat("u", 64)
	tests := []http.Header{
		{},
		{"Authorization": []string{"Bearer " + unrelated}},
		{"Cookie": []string{"Authorization=Bearer " + token}},
		{"Authorization": []string{"Basic " + token}},
		{"Authorization": []string{"Bearer " + token, "Bearer " + token}},
	}
	for _, headers := range tests {
		_, err := authenticator.Authenticate(t.Context(), headers)
		if !errors.Is(err, errDeviceAuthentication) {
			t.Fatalf("unrelated credential unexpectedly authenticated: %v", err)
		}
		if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), unrelated) {
			t.Fatalf("authentication error leaked a credential: %v", err)
		}
	}
}

func TestBrokerEnforcesTokenIdentityAndKeepsDeviceSurfacePrivate(t *testing.T) {
	const token = "connector-token-0123456789abcdef-0123456789abcdef"
	slot := externalgateway.SlotKey{DeviceID: "device-1", SlotID: "codex-1", Runtime: externalgateway.RuntimeCodex}
	authenticator := writeDeviceAuthenticator(t, token, []externalgateway.SlotKey{slot})
	broker, err := externalgateway.NewBroker(externalgateway.Config{}, authenticator)
	if err != nil {
		t.Fatalf("new broker: %v", err)
	}

	tests := []struct {
		name       string
		token      string
		deviceID   string
		wantStatus int
	}{
		{name: "Temporal bearer is not device auth", token: strings.Repeat("t", 64), deviceID: slot.DeviceID, wantStatus: http.StatusUnauthorized},
		{name: "authenticated identity cannot claim another device", token: token, deviceID: "other-device", wantStatus: http.StatusForbidden},
		{name: "explicit device slot connects", token: token, deviceID: slot.DeviceID, wantStatus: http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := externalgateway.ConnectRequest{
				ProtocolVersion: externalgateway.ProtocolVersion,
				DeviceID:        test.deviceID,
				SlotID:          slot.SlotID,
				Runtime:         slot.Runtime,
				MaxConcurrency:  1,
				AllowedPaths:    []string{"/codex/v1/invoke"},
			}
			body, marshalErr := json.Marshal(input)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			request := httptest.NewRequest(http.MethodPost, externalgateway.ConnectPath, bytes.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+test.token)
			response := httptest.NewRecorder()
			broker.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("unexpected status %d, body %s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), token) || strings.Contains(response.Body.String(), test.token) {
				t.Fatal("HTTP response leaked a credential")
			}
		})
	}

	server, err := newExternalGatewayHTTPServer(defaultExternalGatewayBindAddress, broker)
	if err != nil {
		t.Fatalf("new device server: %v", err)
	}
	if server.Addr == ":8083" {
		t.Fatal("device reverse connector reused the health listener")
	}
	request := httptest.NewRequest(http.MethodPost, "/health", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("device listener exposed a non-device route: status %d", response.Code)
	}
}

func TestServeHTTPGracefullyDrainsInFlightRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		writer.WriteHeader(http.StatusNoContent)
	})
	server, err := newExternalGatewayHTTPServer("127.0.0.1:0", handler)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	serveResult := make(chan error, 1)
	go func() { serveResult <- serveHTTP(ctx, server, listener) }()
	requestResult := make(chan error, 1)
	go func() {
		response, requestErr := http.Get("http://" + listener.Addr().String()) //nolint:gosec // loopback test listener
		if requestErr == nil {
			_, requestErr = io.Copy(io.Discard, response.Body)
			requestErr = errors.Join(requestErr, response.Body.Close())
		}
		requestResult <- requestErr
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach server")
	}
	shutdownStarted := make(chan struct{})
	server.RegisterOnShutdown(func() { close(shutdownStarted) })
	cancel()
	select {
	case <-shutdownStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not begin graceful shutdown")
	}
	close(release)

	select {
	case err := <-requestResult:
		if err != nil {
			t.Fatalf("in-flight request failed during graceful shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request was not drained")
	}
	select {
	case err := <-serveResult:
		if err != nil {
			t.Fatalf("server shutdown failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
}

func TestDeviceTokenFileValidationDoesNotRenderSecret(t *testing.T) {
	const weakSecret = "top-secret"
	path := filepath.Join(t.TempDir(), "device-token")
	if err := os.WriteFile(path, []byte(weakSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	authenticator, err := newDeviceTokenAuthenticatorFromFile(path, "device-1", []externalgateway.SlotKey{{DeviceID: "device-1", SlotID: "slot-1", Runtime: externalgateway.RuntimeCodex}})
	if err == nil || authenticator != nil {
		t.Fatal("weak token unexpectedly accepted")
	}
	if strings.Contains(err.Error(), weakSecret) {
		t.Fatal("configuration error rendered token contents")
	}
}

func TestDeviceTokenFileReadIsBoundedAndGrantsOneDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device-token")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxDeviceTokenBytes+100)), 0o600); err != nil {
		t.Fatal(err)
	}
	authenticator, err := newDeviceTokenAuthenticatorFromFile(path, "device-1", []externalgateway.SlotKey{{DeviceID: "device-1", SlotID: "slot-1", Runtime: externalgateway.RuntimeCodex}})
	if err == nil || authenticator != nil {
		t.Fatal("oversized token file unexpectedly accepted")
	}

	const token = "connector-token-0123456789abcdef-0123456789abcdef"
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	authenticator, err = newDeviceTokenAuthenticatorFromFile(path, "device-1", []externalgateway.SlotKey{{DeviceID: "device-2", SlotID: "slot-1", Runtime: externalgateway.RuntimeCodex}})
	if err == nil || authenticator != nil {
		t.Fatal("one token unexpectedly granted a second device identity")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatal("identity validation rendered the token")
	}
}

func mapEnvironment(settings map[string]string) func(string) string {
	return func(name string) string { return settings[name] }
}

func writeDeviceAuthenticator(t *testing.T, token string, slots []externalgateway.SlotKey) *deviceTokenAuthenticator {
	t.Helper()
	path := filepath.Join(t.TempDir(), "device-token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	authenticator, err := newDeviceTokenAuthenticatorFromFile(path, slots[0].DeviceID, slots)
	if err != nil {
		t.Fatalf("load device authenticator: %v", err)
	}
	return authenticator
}
