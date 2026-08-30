//go:build unix

package driver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/harness/runtime"
)

func TestProcessDriverAppServerTurn(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "codex")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  printf '%s\n' 'codex-cli 0.151.0'
  exit 0
fi
trap 'exit 0' INT TERM
IFS= read -r initialize
printf '%s\n' '{"id":1,"result":{"userAgent":"codex-test"}}'
IFS= read -r initialized
IFS= read -r thread
printf '%s\n' '{"id":2,"result":{"thread":{"id":"11111111-1111-4111-8111-111111111111"},"model":"gpt-test","modelProvider":"openai","cwd":"/workspace","approvalPolicy":"never","approvalsReviewer":"user","sandbox":{"type":"dangerFullAccess"}}}'
IFS= read -r turn
printf '%s\n' '{"id":3,"result":{"turn":{"id":"22222222-2222-4222-8222-222222222222","status":"inProgress","items":[]}}}'
printf '%s\n' '{"method":"item/agentMessage/delta","params":{"threadId":"11111111-1111-4111-8111-111111111111","turnId":"22222222-2222-4222-8222-222222222222","itemId":"message-1","delta":"done"}}'
printf '%s\n' '{"method":"item/completed","params":{"threadId":"11111111-1111-4111-8111-111111111111","turnId":"22222222-2222-4222-8222-222222222222","completedAtMs":1,"item":{"type":"agentMessage","id":"message-1","text":"done"}}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"11111111-1111-4111-8111-111111111111","turn":{"id":"22222222-2222-4222-8222-222222222222","status":"completed","items":[]}}}'
if IFS= read -r unexpected; then
  exit 90
fi
exit 0
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	driver := NewProcessDriver(ProcessConfig{
		Executable: executable, ExpectedVersion: "0.151.0", StrictVersion: true,
		Workspace: dir, Model: "gpt-test", ReasoningEffort: "high",
		ModelProvider: "openai", SandboxPolicy: SandboxPolicy{
			ExternalSandbox: &ExternalSandboxPolicy{NetworkAccess: "restricted"},
		},
		Environment: []string{"PATH=/usr/bin:/bin"}, MaxEventBytes: 64 << 10,
		MaxStderrBytes: 8 << 10, HandshakeTimeout: 5 * time.Second, ShutdownGrace: 250 * time.Millisecond,
	})
	validateCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := driver.Validate(validateCtx); err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	outcome, err := driver.Run(context.Background(), runtime.Turn{Prompt: "test"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Failure != nil {
		t.Fatalf("outcome = %#v", outcome)
	}
	if len(sink.sessions) != 1 || sink.sessions[0].ContinuationID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("sessions = %#v", sink.sessions)
	}
	if len(sink.deltas) != 1 || sink.deltas[0].Text != "done" {
		t.Fatalf("deltas = %#v", sink.deltas)
	}
}

func TestProcessDriverCancellationClosesBlockedTurnWrite(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "codex")
	script := `#!/bin/sh
trap 'exit 0' INT TERM
IFS= read -r initialize
printf '%s\n' '{"id":1,"result":{"userAgent":"codex-test"}}'
IFS= read -r initialized
IFS= read -r thread
printf '%s\n' '{"id":2,"result":{"thread":{"id":"11111111-1111-4111-8111-111111111111"}}}'
sleep 30
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	driver := NewProcessDriver(ProcessConfig{
		Executable: executable, Workspace: dir, Model: "gpt-test", ModelProvider: "openai",
		SandboxPolicy: SandboxPolicy{ExternalSandbox: &ExternalSandboxPolicy{NetworkAccess: "restricted"}},
		Environment:   []string{"PATH=/usr/bin:/bin"}, MaxEventBytes: 64 << 10, MaxStderrBytes: 8 << 10,
		HandshakeTimeout: 5 * time.Second, ShutdownGrace: 100 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := driver.Run(ctx, runtime.Turn{Prompt: strings.Repeat("x", 900<<10)}, &recordingSink{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("blocked turn write returned after %s", elapsed)
	}
}

func TestProcessDriverCredentialIsolationPreflight(t *testing.T) {
	tests := []struct {
		name      string
		exitCode  string
		wantError bool
	}{
		{name: "credential denied and workspace writable", exitCode: "43"},
		{name: "credential readable", exitCode: "42", wantError: true},
		{name: "credential readable through workspace symlink", exitCode: "44", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(root, "workspace")
			codexHome := filepath.Join(root, "codex")
			for _, path := range []string{workspace, codexHome} {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			credential := filepath.Join(codexHome, "auth.json")
			if err := os.WriteFile(credential, []byte("host-managed-auth"), 0o600); err != nil {
				t.Fatal(err)
			}
			executable := filepath.Join(root, "codex-bin")
			script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  printf '%s\n' 'codex-cli 0.151.0'
  exit 0
fi
trap 'exit 0' INT TERM
IFS= read -r initialize
printf '%s\n' '{"id":1,"result":{"codexHome":"'"$TEST_CODEX_HOME"'"}}'
IFS= read -r initialized
IFS= read -r profiles
printf '%s\n' '{"id":2,"result":{"data":[{"id":"yourown_chat_local","allowed":true}],"nextCursor":null}}'
IFS= read -r command
printf '%s\n' '{"id":3,"result":{"exitCode":'"$TEST_PROBE_EXIT"',"stdout":"","stderr":""}}'
IFS= read -r unexpected || true
exit 0
`
			if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			driver := NewProcessDriver(ProcessConfig{
				Executable: executable, ExpectedVersion: "0.151.0", StrictVersion: true,
				Workspace: workspace, Model: "gpt-test", ModelProvider: "openai",
				SandboxPolicy:       SandboxPolicy{PermissionProfile: &PermissionProfilePolicy{ID: "yourown_chat_local"}},
				CredentialReadProbe: credential,
				Environment: []string{
					"PATH=/usr/bin:/bin", "TEST_CODEX_HOME=" + codexHome, "TEST_PROBE_EXIT=" + tt.exitCode,
				},
				MaxEventBytes: 64 << 10, MaxStderrBytes: 8 << 10,
				HandshakeTimeout: 5 * time.Second, ShutdownGrace: 250 * time.Millisecond,
			})
			err := driver.Validate(context.Background())
			if tt.wantError {
				if !errors.Is(err, ErrCredentialIsolation) {
					t.Fatalf("Validate() error = %v, want ErrCredentialIsolation", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSameExistingPathResolvesAncestorSymlinks(t *testing.T) {
	realDirectory := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realDirectory, alias); err != nil {
		t.Fatal(err)
	}
	if !sameExistingPath(realDirectory, alias) {
		t.Fatalf("sameExistingPath(%q, %q) = false", realDirectory, alias)
	}
}

func TestOpenThreadPinsNamedPermissionProfileAndWorkspace(t *testing.T) {
	workspace := t.TempDir()
	for _, continuationID := range []string{"", "existing-thread"} {
		t.Run(map[bool]string{true: "start", false: "resume"}[continuationID == ""], func(t *testing.T) {
			threadID := continuationID
			if threadID == "" {
				threadID = "new-thread"
			}
			result, err := json.Marshal(map[string]any{
				"thread":                  map[string]string{"id": threadID},
				"runtimeWorkspaceRoots":   []string{workspace},
				"activePermissionProfile": map[string]string{"id": "yourown_chat_local"},
			})
			if err != nil {
				t.Fatal(err)
			}
			messages := make(chan readResult, 1)
			messages <- readResult{message: rpcMessage{ID: json.RawMessage(`2`), Result: result}}
			output := &testWriteCloser{}
			client := protocolClient{
				input: output, messages: messages,
				maxRequestBytes: 1 << 20, maxPendingBytes: 1 << 20, writeTimeout: time.Second,
			}
			driver := NewProcessDriver(ProcessConfig{
				Workspace: workspace, Model: "gpt-test", ModelProvider: "openai",
				SandboxPolicy:    SandboxPolicy{PermissionProfile: &PermissionProfilePolicy{ID: "yourown_chat_local"}},
				HandshakeTimeout: 5 * time.Second,
			})
			gotThread, err := driver.openThread(context.Background(), &client, continuationID)
			if err != nil || gotThread != threadID {
				t.Fatalf("openThread() = %q, %v", gotThread, err)
			}
			var envelope struct {
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			wantMethod := "thread/start"
			if continuationID != "" {
				wantMethod = "thread/resume"
			}
			if envelope.Method != wantMethod || envelope.Params["permissions"] != "yourown_chat_local" || envelope.Params["approvalPolicy"] != "never" {
				t.Fatalf("thread request = %#v", envelope)
			}
			roots, ok := envelope.Params["runtimeWorkspaceRoots"].([]any)
			if !ok || len(roots) != 1 || roots[0] != workspace {
				t.Fatalf("runtime workspace roots = %#v", envelope.Params["runtimeWorkspaceRoots"])
			}
			for _, forbidden := range []string{"sandbox", "sandboxPolicy", "config"} {
				if _, exists := envelope.Params[forbidden]; exists {
					t.Fatalf("thread request exposed forbidden field %q", forbidden)
				}
			}
		})
	}
}

func TestCancelTurnRequiresInterruptAckAndMatchingCompletion(t *testing.T) {
	messages := make(chan readResult, 4)
	output := &testWriteCloser{}
	client := protocolClient{
		input: output, messages: messages,
		maxRequestBytes: 1 << 20, maxPendingBytes: 1 << 20, writeTimeout: time.Second,
	}
	driver := NewProcessDriver(ProcessConfig{ShutdownGrace: time.Second})
	done := make(chan bool, 1)
	go func() { done <- driver.cancelTurn(&client, "thread-1", "turn-1") }()
	messages <- readResult{message: rpcMessage{ID: json.RawMessage(`4`), Result: json.RawMessage(`{}`)}}
	unrelated, err := json.Marshal(map[string]any{
		"threadId": "thread-1", "turn": map[string]string{"id": "turn-other", "status": "interrupted"},
	})
	if err != nil {
		t.Fatal(err)
	}
	messages <- readResult{message: rpcMessage{Method: turnCompletedMethod, Params: unrelated}}
	select {
	case result := <-done:
		t.Fatalf("cancelTurn returned early: %t", result)
	case <-time.After(50 * time.Millisecond):
	}
	matching, err := json.Marshal(map[string]any{
		"threadId": "thread-1", "turn": map[string]string{"id": "turn-1", "status": "interrupted"},
	})
	if err != nil {
		t.Fatal(err)
	}
	messages <- readResult{message: rpcMessage{Method: turnCompletedMethod, Params: matching}}
	select {
	case result := <-done:
		if !result {
			t.Fatal("cancelTurn did not recognize correlated graceful cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelTurn did not return after correlated completion")
	}
	if !strings.Contains(output.String(), `"method":"turn/interrupt"`) {
		t.Fatalf("interrupt request = %q", output.String())
	}
}

func TestVersionProbeIsBoundedSanitizedAndKillsDescendants(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "codex")
	pidFile := filepath.Join(dir, "child.pid")
	const diagnostic = "credential-shaped-version-output"
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  sleep 30 &
  child=$!
  printf '%s\n' "$child" > "$PID_FILE"
  printf '%s\n' '` + diagnostic + `'
  exit 0
fi
exit 1
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	driver := NewProcessDriver(ProcessConfig{
		Executable: executable, ExpectedVersion: "0.151.0", StrictVersion: true,
		Workspace: dir, Model: "gpt-test", ModelProvider: "openai",
		SandboxPolicy: SandboxPolicy{ExternalSandbox: &ExternalSandboxPolicy{NetworkAccess: "restricted"}},
		Environment:   []string{"PATH=/usr/bin:/bin", "PID_FILE=" + pidFile},
		MaxEventBytes: 1 << 20, MaxStderrBytes: 8 << 10,
		HandshakeTimeout: 5 * time.Second, ShutdownGrace: 100 * time.Millisecond,
	})
	err := driver.Validate(context.Background())
	if err == nil || strings.Contains(err.Error(), diagnostic) || !strings.Contains(err.Error(), "version mismatch") {
		t.Fatalf("Validate() error = %v", err)
	}
	rawPID, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processExists(pid) {
		t.Fatalf("version probe descendant %d remained alive", pid)
	}
}

func TestProcessDriverProtocolFailureTerminatesLiveProcessGroup(t *testing.T) {
	tests := []struct {
		name      string
		badLine   string
		maxBytes  int
		wantError string
	}{
		{name: "malformed JSON", badLine: `{not-json}`, maxBytes: 1 << 10, wantError: "decode Codex app-server message"},
		{
			name: "oversized JSON", badLine: `{"method":"` + strings.Repeat("x", 256) + `"}`,
			maxBytes: 64, wantError: "message exceeds 64 bytes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			executable := filepath.Join(dir, "codex")
			pidFile := filepath.Join(dir, "pids")
			script := `#!/bin/sh
trap 'exit 0' INT TERM
IFS= read -r initialize
sleep 30 &
child=$!
printf '%s %s\n' "$$" "$child" > "$PID_FILE"
printf '%s\n' "$BAD_LINE"
wait "$child"
`
			if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			driver := NewProcessDriver(ProcessConfig{
				Executable: executable, ExpectedVersion: "0.151.0", StrictVersion: true,
				Workspace: dir, Model: "gpt-test", ModelProvider: "openai",
				SandboxPolicy: SandboxPolicy{
					ExternalSandbox: &ExternalSandboxPolicy{NetworkAccess: "restricted"},
				},
				Environment: []string{
					"PATH=/usr/bin:/bin", "PID_FILE=" + pidFile, "BAD_LINE=" + tt.badLine,
				},
				MaxEventBytes: tt.maxBytes, MaxStderrBytes: 8 << 10,
				HandshakeTimeout: 5 * time.Second, ShutdownGrace: 100 * time.Millisecond,
			})

			started := time.Now()
			_, err := driver.Run(context.Background(), runtime.Turn{Prompt: "test"}, &recordingSink{})
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Run() error = %v, want error containing %q", err, tt.wantError)
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("Run() returned after %s; protocol failure waited for live app-server", elapsed)
			}

			rawPIDs, readErr := os.ReadFile(pidFile)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for rawPID := range strings.FieldsSeq(string(rawPIDs)) {
				pid, parseErr := strconv.Atoi(rawPID)
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				if processExists(pid) {
					t.Fatalf("process %d remained alive after Run returned", pid)
				}
			}
		})
	}
}

func TestProcessDriverDoesNotReflectStderr(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "codex")
	const diagnostic = "credential-shaped-vendor-diagnostic"
	script := "#!/bin/sh\nprintf '%s\\n' '" + diagnostic + "' >&2\nexit 17\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	driver := NewProcessDriver(ProcessConfig{
		Executable: executable, Workspace: dir, Model: "gpt-test", ModelProvider: "openai",
		SandboxPolicy: SandboxPolicy{ExternalSandbox: &ExternalSandboxPolicy{NetworkAccess: "restricted"}},
		Environment:   []string{"PATH=/usr/bin:/bin"}, MaxEventBytes: 1024, MaxStderrBytes: 1024,
		HandshakeTimeout: 5 * time.Second, ShutdownGrace: 100 * time.Millisecond,
	})
	_, err := driver.Run(context.Background(), runtime.Turn{Prompt: "test"}, &recordingSink{})
	if err == nil || strings.Contains(err.Error(), diagnostic) {
		t.Fatalf("Run() error = %v", err)
	}
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
