package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadJSONL(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)
	results := readJSONL(strings.NewReader("{\"id\":1,\"result\":{}}\n{\"method\":\"initialized\",\"params\":{}}\n"), 1024, stop)
	first := <-results
	if first.err != nil || string(first.message.ID) != "1" {
		t.Fatalf("first message = %#v, error %v", first.message, first.err)
	}
	second := <-results
	if second.err != nil || second.message.Method != "initialized" {
		t.Fatalf("second message = %#v, error %v", second.message, second.err)
	}
	terminal := <-results
	if terminal.err == nil {
		t.Fatal("reader did not report EOF")
	}
}

func TestReadJSONLRejectsOversizedAndMalformedMessages(t *testing.T) {
	for name, input := range map[string]string{
		"oversized":      `{"method":"` + strings.Repeat("x", 64) + `"}`,
		"malformed":      `{not-json}`,
		"empty envelope": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			stop := make(chan struct{})
			defer close(stop)
			result := <-readJSONL(strings.NewReader(input+"\n"), 32, stop)
			if result.err == nil {
				t.Fatalf("readJSONL accepted %q", input)
			}
		})
	}
}

func TestWriteJSONL(t *testing.T) {
	var output bytes.Buffer
	if err := writeJSONL(&output, request(7, "thread/start", map[string]string{"model": "gpt"})); err != nil {
		t.Fatal(err)
	}
	want := "{\"id\":7,\"method\":\"thread/start\",\"params\":{\"model\":\"gpt\"}}\n"
	if output.String() != want {
		t.Fatalf("writeJSONL() = %q, want %q", output.String(), want)
	}
}

func TestWriteJSONLContextBoundsBlockedAndOversizedRequests(t *testing.T) {
	blocked := &blockingWriteCloser{closed: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := writeJSONLContext(ctx, blocked, 1024, request(1, "turn/start", map[string]string{"prompt": "work"}))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("writeJSONLContext() error = %v, want deadline", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("blocked protocol write ignored its context")
	}
	select {
	case <-blocked.closed:
	default:
		t.Fatal("blocked protocol write did not close app-server stdin")
	}

	output := &testWriteCloser{}
	if err := writeJSONLContext(context.Background(), output, 8, request(1, "initialize", struct{}{})); err == nil || !strings.Contains(err.Error(), "exceeds 8 bytes") {
		t.Fatalf("oversized write error = %v", err)
	}
	if output.Len() != 0 {
		t.Fatal("oversized protocol request reached app-server stdin")
	}
}

func TestProtocolClientBoundsPendingHandshakeNotifications(t *testing.T) {
	messages := make(chan readResult, maxPendingHandshakeMessages+1)
	for range maxPendingHandshakeMessages + 1 {
		messages <- readResult{message: rpcMessage{Method: "item/started", Params: json.RawMessage(`{}`)}}
	}
	client := protocolClient{
		input: &testWriteCloser{}, messages: messages,
		maxRequestBytes: 1024, maxPendingBytes: 1 << 20, writeTimeout: time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := client.awaitResponse(ctx, 1); err == nil || !strings.Contains(err.Error(), "too many pending notifications") {
		t.Fatalf("awaitResponse() error = %v", err)
	}
}

func TestPinnedTurnStartPayloadUsesSelectedSandbox(t *testing.T) {
	model, effort, tier := "gpt-5.6-codex", "high", "fast"
	tests := []struct {
		name   string
		policy SandboxPolicy
		want   string
	}{
		{
			name: "external sandbox",
			policy: SandboxPolicy{
				ExternalSandbox: &ExternalSandboxPolicy{NetworkAccess: "restricted"},
			},
			want: `{"threadId":"thread-1","input":[{"type":"text","text":"work"}],"model":"gpt-5.6-codex","effort":"high","serviceTier":"fast","approvalPolicy":"never","sandboxPolicy":{"type":"externalSandbox","networkAccess":"restricted"}}`,
		},
		{
			name:   "named permission profile",
			policy: SandboxPolicy{PermissionProfile: &PermissionProfilePolicy{ID: "yourown_chat_local"}},
			want:   `{"threadId":"thread-1","input":[{"type":"text","text":"work"}],"model":"gpt-5.6-codex","effort":"high","serviceTier":"fast","approvalPolicy":"never"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := turnStartParams{
				ThreadID: "thread-1", Input: []userInput{{Type: "text", Text: "work"}},
				Model: &model, Effort: &effort, ServiceTier: &tier, ApprovalPolicy: "never",
				SandboxPolicy: tt.policy.appServerPolicy(),
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tt.want {
				t.Fatalf("turn/start payload = %s, want %s", raw, tt.want)
			}
		})
	}
}

func TestProcessDriverRejectsInvalidSandboxPolicy(t *testing.T) {
	tests := []struct {
		name      string
		workspace string
		policy    SandboxPolicy
		wantError string
	}{
		{name: "missing", workspace: "/workspace", wantError: "exactly one"},
		{
			name: "multiple", workspace: "/workspace",
			policy: SandboxPolicy{
				ExternalSandbox:   &ExternalSandboxPolicy{NetworkAccess: "restricted"},
				PermissionProfile: &PermissionProfilePolicy{ID: "yourown_chat_local"},
			},
			wantError: "exactly one",
		},
		{
			name: "invalid external network", workspace: "/workspace",
			policy:    SandboxPolicy{ExternalSandbox: &ExternalSandboxPolicy{NetworkAccess: "true"}},
			wantError: "must be restricted or enabled",
		},
		{
			name: "relative permission profile workspace", workspace: "workspace",
			policy:    SandboxPolicy{PermissionProfile: &PermissionProfilePolicy{ID: "yourown_chat_local"}},
			wantError: "must be an absolute path",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			driver := NewProcessDriver(ProcessConfig{
				Executable: "codex", Workspace: tt.workspace, Model: "gpt-test", ModelProvider: "openai",
				SandboxPolicy: tt.policy, MaxEventBytes: 1, MaxStderrBytes: 1,
				HandshakeTimeout: time.Second, ShutdownGrace: time.Second,
			})
			err := driver.validateConfig()
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("validateConfig() error = %v, want error containing %q", err, tt.wantError)
			}
		})
	}
}

func TestServerApprovalRequestsFailClosed(t *testing.T) {
	tests := []struct {
		method       string
		wantDecision string
		wantError    float64
	}{
		{method: "item/commandExecution/requestApproval", wantDecision: "cancel"},
		{method: "item/fileChange/requestApproval", wantDecision: "cancel"},
		{method: "applyPatchApproval", wantDecision: "abort"},
		{method: "execCommandApproval", wantDecision: "abort"},
		{method: "item/permissions/requestApproval", wantError: -32001},
		{method: "unknown/request", wantError: -32601},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			output := &testWriteCloser{}
			client := protocolClient{input: output, maxRequestBytes: 1024, writeTimeout: time.Second}
			if err := client.answerServerRequest(context.Background(), rpcMessage{ID: json.RawMessage(`9`), Method: tt.method}); err != nil {
				t.Fatal(err)
			}
			var envelope map[string]any
			if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if tt.wantDecision != "" {
				result, ok := envelope["result"].(map[string]any)
				if !ok || result["decision"] != tt.wantDecision {
					t.Fatalf("response = %#v", envelope)
				}
				return
			}
			rpcError, ok := envelope["error"].(map[string]any)
			if !ok || rpcError["code"] != tt.wantError {
				t.Fatalf("response = %#v", envelope)
			}
		})
	}
}

type testWriteCloser struct {
	bytes.Buffer
}

func (*testWriteCloser) Close() error { return nil }

type blockingWriteCloser struct {
	once   sync.Once
	closed chan struct{}
}

func (writer *blockingWriteCloser) Write([]byte) (int, error) {
	<-writer.closed
	return 0, io.ErrClosedPipe
}

func (writer *blockingWriteCloser) Close() error {
	writer.once.Do(func() { close(writer.closed) })
	return nil
}
