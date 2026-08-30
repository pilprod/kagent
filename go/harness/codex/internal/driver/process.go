package driver

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	gostdruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/kagent-dev/kagent/go/harness/runtime"
)

const (
	initializeRequestID = 1
	threadRequestID     = 2
	turnRequestID       = 3
	interruptRequestID  = 4
)

// ErrCredentialIsolation reports a failed runtime proof that sandboxed Codex
// commands cannot open the host-managed subscription credential file.
var ErrCredentialIsolation = errors.New("codex credential read isolation is unavailable")

// SandboxPolicy selects exactly one app-server sandbox contract.
type SandboxPolicy struct {
	ExternalSandbox   *ExternalSandboxPolicy
	PermissionProfile *PermissionProfilePolicy
}

// ExternalSandboxPolicy delegates filesystem isolation to an attested outer
// sandbox such as gVisor or Docker while preserving Codex network policy.
type ExternalSandboxPolicy struct {
	NetworkAccess string
}

// PermissionProfilePolicy selects one compiler-owned named profile. The
// app-server must confirm this exact profile before any turn can begin.
type PermissionProfilePolicy struct {
	ID string
}

// ProcessConfig describes one isolated Codex app-server runtime.
type ProcessConfig struct {
	Executable            string
	ExpectedVersion       string
	StrictVersion         bool
	Workspace             string
	Model                 string
	ModelProvider         string
	ReasoningEffort       string
	ServiceTier           string
	DeveloperInstructions string
	SandboxPolicy         SandboxPolicy
	CredentialReadProbe   string
	Environment           []string
	MaxEventBytes         int
	MaxStderrBytes        int
	HandshakeTimeout      time.Duration
	ShutdownGrace         time.Duration
}

// ProcessDriver executes Codex turns through the official app-server protocol.
type ProcessDriver struct {
	config ProcessConfig
}

// NewProcessDriver constructs a Codex process driver.
func NewProcessDriver(config ProcessConfig) *ProcessDriver {
	return &ProcessDriver{config: config}
}

// Validate checks that the configured Codex executable is present and pinned.
func (d *ProcessDriver) Validate(ctx context.Context) error {
	if err := d.validateConfig(); err != nil {
		return err
	}
	path, err := exec.LookPath(d.config.Executable)
	if err != nil {
		return fmt.Errorf("find Codex executable %q: %w", d.config.Executable, err)
	}
	if err := d.validateVersion(ctx, path); err != nil {
		return err
	}
	if d.config.SandboxPolicy.PermissionProfile != nil {
		if err := d.validateCredentialIsolation(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (d *ProcessDriver) validateVersion(ctx context.Context, path string) error {
	versionCtx, cancel := context.WithTimeout(ctx, d.config.HandshakeTimeout)
	defer cancel()
	if err := versionCtx.Err(); err != nil {
		return fmt.Errorf("read Codex version: %w", err)
	}
	output, err := os.CreateTemp("", ".kagent-codex-version-*")
	if err != nil {
		return fmt.Errorf("prepare Codex version output: %w", err)
	}
	outputPath := output.Name()
	defer os.Remove(outputPath)
	defer output.Close()
	if err := output.Chmod(0o600); err != nil {
		return fmt.Errorf("protect Codex version output: %w", err)
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open Codex version diagnostic sink: %w", err)
	}
	defer devNull.Close()

	cmd := exec.Command(path, "--version")
	configureProcessGroup(cmd)
	cmd.Dir = d.config.Workspace
	cmd.Env = append([]string(nil), d.config.Environment...)
	cmd.Stdout = output
	cmd.Stderr = devNull
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Codex version probe: %w", err)
	}
	waiter := newProcessWaiter(cmd)
	waiter.start()
	select {
	case <-waiter.done:
	case <-versionCtx.Done():
		_ = killProcessGroup(cmd.Process)
		<-waiter.done
		return fmt.Errorf("read Codex version: %w", versionCtx.Err())
	}
	// A version shim must not leave descendants running after its parent exits.
	_ = killProcessGroup(cmd.Process)
	if waiter.err != nil {
		return fmt.Errorf("read Codex version: process failed")
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("read Codex version output: %w", err)
	}
	const maxVersionBytes = 1024
	raw, err := io.ReadAll(io.LimitReader(output, maxVersionBytes+1))
	if err != nil {
		return fmt.Errorf("read Codex version output: %w", err)
	}
	defer clear(raw)
	if len(raw) > maxVersionBytes {
		return fmt.Errorf("codex version output exceeds %d bytes", maxVersionBytes)
	}
	wantVersion := "codex-cli " + d.config.ExpectedVersion
	if d.config.StrictVersion && strings.TrimSpace(string(raw)) != wantVersion {
		return fmt.Errorf("codex version mismatch: expected %q", wantVersion)
	}
	return nil
}

func (d *ProcessDriver) validateCredentialIsolation(ctx context.Context) (runErr error) {
	if gostdruntime.GOOS == "windows" {
		return fmt.Errorf("%w: native Windows conformance is not implemented", ErrCredentialIsolation)
	}
	credentialBefore, err := os.Lstat(d.config.CredentialReadProbe)
	if err != nil || credentialBefore.Mode()&os.ModeSymlink != 0 || !credentialBefore.Mode().IsRegular() {
		return fmt.Errorf("%w: credential probe target is unavailable", ErrCredentialIsolation)
	}
	credentialDigestBefore, err := fileSHA256(d.config.CredentialReadProbe)
	if err != nil {
		return fmt.Errorf("%w: fingerprint credential probe target", ErrCredentialIsolation)
	}
	random := make([]byte, 16)
	if _, err := cryptorand.Read(random); err != nil {
		return fmt.Errorf("%w: create workspace probe", ErrCredentialIsolation)
	}
	probePath := filepath.Join(d.config.Workspace, ".yourown-chat-codex-permission-probe-"+hex.EncodeToString(random))
	credentialLink := probePath + "-credential-link"
	clear(random)
	if _, err := os.Lstat(probePath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: workspace probe path is not fresh", ErrCredentialIsolation)
	}
	defer os.Remove(probePath)
	if _, err := os.Lstat(credentialLink); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: credential symlink probe path is not fresh", ErrCredentialIsolation)
	}
	if err := os.Symlink(d.config.CredentialReadProbe, credentialLink); err != nil {
		return fmt.Errorf("%w: create credential symlink probe", ErrCredentialIsolation)
	}
	defer os.Remove(credentialLink)

	cmd := exec.Command(d.config.Executable, "app-server", "--listen", "stdio://", "--strict-config")
	configureProcessGroup(cmd)
	cmd.Dir = d.config.Workspace
	cmd.Env = append([]string(nil), d.config.Environment...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("%w: open app-server stdin", ErrCredentialIsolation)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%w: open app-server stdout", ErrCredentialIsolation)
	}
	stderr := &boundedBuffer{max: d.config.MaxStderrBytes}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: start app-server probe", ErrCredentialIsolation)
	}
	stopReader := make(chan struct{})
	messages := readJSONL(stdout, d.config.MaxEventBytes, stopReader)
	waiter := newProcessWaiter(cmd)
	defer func() {
		close(stopReader)
		_ = stdin.Close()
		if runErr == nil {
			d.shutdown(cmd, waiter)
		} else {
			d.terminate(cmd, waiter)
		}
		_ = killProcessGroup(cmd.Process)
	}()

	client := d.newProtocolClient(stdin, messages, waiter)
	if err := client.write(ctx, request(initializeRequestID, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "kagent", "title": "Kagent Codex Harness", "version": "0.1.0"},
		"capabilities": map[string]bool{"experimentalApi": true},
	})); err != nil {
		return fmt.Errorf("%w: initialize app-server probe", ErrCredentialIsolation)
	}
	initializeResult, _, err := d.awaitResponse(ctx, &client, initializeRequestID, "credential probe initialize")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCredentialIsolation, err)
	}
	var initialized struct {
		CodexHome string `json:"codexHome"`
	}
	if err := json.Unmarshal(initializeResult, &initialized); err != nil || !sameExistingPath(initialized.CodexHome, filepath.Dir(d.config.CredentialReadProbe)) {
		return fmt.Errorf("%w: app-server reported an unexpected CODEX_HOME", ErrCredentialIsolation)
	}
	if err := client.write(ctx, notification("initialized", struct{}{})); err != nil {
		return fmt.Errorf("%w: acknowledge app-server probe", ErrCredentialIsolation)
	}

	const permissionListRequestID = 2
	if err := client.write(ctx, request(permissionListRequestID, "permissionProfile/list", map[string]string{"cwd": d.config.Workspace})); err != nil {
		return fmt.Errorf("%w: list permission profiles", ErrCredentialIsolation)
	}
	listResult, _, err := d.awaitResponse(ctx, &client, permissionListRequestID, "permission profile list")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCredentialIsolation, err)
	}
	var catalog struct {
		Data []struct {
			ID      string `json:"id"`
			Allowed bool   `json:"allowed"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listResult, &catalog); err != nil {
		return fmt.Errorf("%w: decode permission profile catalog", ErrCredentialIsolation)
	}
	profileAllowed := false
	for _, profile := range catalog.Data {
		if profile.ID == d.config.SandboxPolicy.PermissionProfile.ID && profile.Allowed {
			profileAllowed = true
			break
		}
	}
	if !profileAllowed {
		return fmt.Errorf("%w: selected permission profile is unavailable", ErrCredentialIsolation)
	}

	const commandRequestID = 3
	const probeScript = `probe=$1
secret=$2
secret_link=$3
(umask 077; : > "$probe") || exit 40
rm -f "$probe" || exit 41
if (exec 3<"$secret") 2>/dev/null; then
  exit 42
fi
if (exec 3<"$secret_link") 2>/dev/null; then
  exit 44
fi
exit 43`
	if err := client.write(ctx, request(commandRequestID, "command/exec", map[string]any{
		"command": []string{"/bin/sh", "-c", probeScript, "kagent-credential-probe", probePath, d.config.CredentialReadProbe, credentialLink},
		"cwd":     d.config.Workspace, "timeoutMs": 5_000, "outputBytesCap": 4_096,
	})); err != nil {
		return fmt.Errorf("%w: start sandbox read probe", ErrCredentialIsolation)
	}
	commandResult, _, err := d.awaitResponse(ctx, &client, commandRequestID, "sandbox read probe")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCredentialIsolation, err)
	}
	var command struct {
		ExitCode int    `json:"exitCode"`
		Stdout   string `json:"stdout"`
	}
	if err := json.Unmarshal(commandResult, &command); err != nil {
		return fmt.Errorf("%w: decode sandbox read probe", ErrCredentialIsolation)
	}
	if command.ExitCode != 43 || command.Stdout != "" {
		return fmt.Errorf("%w: sandbox read probe returned exit code %d", ErrCredentialIsolation, command.ExitCode)
	}
	if _, err := os.Lstat(probePath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: workspace probe cleanup failed", ErrCredentialIsolation)
	}
	credentialAfter, err := os.Lstat(d.config.CredentialReadProbe)
	if err != nil || !os.SameFile(credentialBefore, credentialAfter) {
		return fmt.Errorf("%w: credential probe target changed", ErrCredentialIsolation)
	}
	credentialDigestAfter, err := fileSHA256(d.config.CredentialReadProbe)
	if err != nil || credentialDigestBefore != credentialDigestAfter {
		return fmt.Errorf("%w: credential probe contents changed", ErrCredentialIsolation)
	}
	return nil
}

// Run executes one A2A turn against a new or durable Codex thread.
func (d *ProcessDriver) Run(ctx context.Context, turn runtime.Turn, sink runtime.EventSink) (outcome runtime.Outcome, runErr error) {
	if err := d.validateTurn(turn, sink); err != nil {
		return runtime.Outcome{}, err
	}
	cmd := exec.Command(d.config.Executable, "app-server", "--listen", "stdio://", "--strict-config")
	configureProcessGroup(cmd)
	cmd.Dir = d.config.Workspace
	cmd.Env = append([]string(nil), d.config.Environment...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return runtime.Outcome{}, fmt.Errorf("open Codex stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runtime.Outcome{}, fmt.Errorf("open Codex stdout: %w", err)
	}
	stderr := &boundedBuffer{max: d.config.MaxStderrBytes}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return runtime.Outcome{}, fmt.Errorf("start Codex app-server: %w", err)
	}

	stopReader := make(chan struct{})
	messages := readJSONL(stdout, d.config.MaxEventBytes, stopReader)
	waiter := newProcessWaiter(cmd)
	gracefulCancellation := false
	defer func() {
		close(stopReader)
		_ = stdin.Close()
		if runErr == nil || gracefulCancellation {
			d.shutdown(cmd, waiter)
		} else {
			d.terminate(cmd, waiter)
		}
		// The app-server may leave descendants behind after its protocol process
		// exits. Always close the entire isolated process group before returning.
		_ = killProcessGroup(cmd.Process)
	}()

	client := d.newProtocolClient(stdin, messages, waiter)
	if err := client.write(ctx, request(initializeRequestID, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "kagent", "title": "Kagent Codex Harness", "version": "0.1.0"},
		"capabilities": map[string]bool{"experimentalApi": true},
	})); err != nil {
		return runtime.Outcome{}, err
	}
	if _, _, err := d.awaitResponse(ctx, &client, initializeRequestID, "initialize"); err != nil {
		return runtime.Outcome{}, err
	}
	if err := client.write(ctx, notification("initialized", struct{}{})); err != nil {
		return runtime.Outcome{}, err
	}

	threadID, err := d.openThread(ctx, &client, turn.ContinuationID)
	if err != nil {
		return runtime.Outcome{}, err
	}
	if err := sink.SessionStarted(runtime.SessionStarted{ContinuationID: threadID}); err != nil {
		return runtime.Outcome{}, err
	}

	turnParams := turnStartParams{
		ThreadID:       threadID,
		Input:          []userInput{{Type: "text", Text: turn.Prompt}},
		ApprovalPolicy: "never",
		SandboxPolicy:  d.config.SandboxPolicy.appServerPolicy(),
	}
	if d.config.Model != "" {
		turnParams.Model = &d.config.Model
	}
	if d.config.ReasoningEffort != "" {
		turnParams.Effort = &d.config.ReasoningEffort
	}
	if d.config.ServiceTier != "" {
		turnParams.ServiceTier = &d.config.ServiceTier
	}
	if err := client.write(ctx, request(turnRequestID, "turn/start", turnParams)); err != nil {
		return runtime.Outcome{}, err
	}
	result, pending, err := d.awaitResponse(ctx, &client, turnRequestID, "turn/start")
	if err != nil {
		return runtime.Outcome{}, err
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(result, &started); err != nil {
		return runtime.Outcome{}, fmt.Errorf("decode Codex turn/start response: %w", err)
	}
	if started.Turn.ID == "" {
		return runtime.Outcome{}, fmt.Errorf("codex turn/start response omitted turn id")
	}
	events := newTurnEvents(threadID, started.Turn.ID, sink)
	for _, message := range pending {
		outcome, terminal, err := events.handle(message)
		if err != nil {
			return runtime.Outcome{}, err
		}
		if terminal {
			return *outcome, nil
		}
	}

	for {
		message, err := client.next(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				gracefulCancellation = d.cancelTurn(&client, threadID, started.Turn.ID)
			}
			return runtime.Outcome{}, err
		}
		if len(message.ID) != 0 && message.Method != "" {
			if err := client.answerServerRequest(ctx, message); err != nil {
				return runtime.Outcome{}, err
			}
			continue
		}
		if message.Method == "" {
			return runtime.Outcome{}, fmt.Errorf("unexpected Codex response outside a request")
		}
		outcome, terminal, err := events.handle(message)
		if err != nil {
			return runtime.Outcome{}, err
		}
		if terminal {
			return *outcome, nil
		}
	}
}

func (d *ProcessDriver) openThread(ctx context.Context, client *protocolClient, continuationID string) (string, error) {
	permissionProfile := ""
	var runtimeWorkspaceRoots []string
	if profile := d.config.SandboxPolicy.PermissionProfile; profile != nil {
		permissionProfile = profile.ID
		runtimeWorkspaceRoots = []string{d.config.Workspace}
	}
	var params any
	if continuationID == "" {
		params = threadStartParams{
			Model: d.config.Model, CWD: d.config.Workspace,
			ModelProvider:         d.config.ModelProvider,
			DeveloperInstructions: d.config.DeveloperInstructions,
			ApprovalPolicy:        "never",
			ServiceTier:           d.config.ServiceTier,
			Permissions:           permissionProfile,
			RuntimeWorkspaceRoots: runtimeWorkspaceRoots,
		}
		if err := client.write(ctx, request(threadRequestID, "thread/start", params)); err != nil {
			return "", err
		}
	} else {
		params = threadResumeParams{
			ThreadID: continuationID, Model: d.config.Model, CWD: d.config.Workspace,
			ModelProvider:         d.config.ModelProvider,
			DeveloperInstructions: d.config.DeveloperInstructions,
			ApprovalPolicy:        "never",
			ServiceTier:           d.config.ServiceTier,
			Permissions:           permissionProfile,
			RuntimeWorkspaceRoots: runtimeWorkspaceRoots,
		}
		if err := client.write(ctx, request(threadRequestID, "thread/resume", params)); err != nil {
			return "", err
		}
	}
	result, _, err := d.awaitResponse(ctx, client, threadRequestID, "thread")
	if err != nil {
		return "", err
	}
	var opened struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		RuntimeWorkspaceRoots   []string `json:"runtimeWorkspaceRoots"`
		ActivePermissionProfile *struct {
			ID string `json:"id"`
		} `json:"activePermissionProfile"`
	}
	if err := json.Unmarshal(result, &opened); err != nil {
		return "", fmt.Errorf("decode Codex thread response: %w", err)
	}
	if opened.Thread.ID == "" {
		return "", fmt.Errorf("codex thread response omitted thread id")
	}
	if continuationID != "" && opened.Thread.ID != continuationID {
		return "", fmt.Errorf("codex resumed thread %q instead of %q", opened.Thread.ID, continuationID)
	}
	if profile := d.config.SandboxPolicy.PermissionProfile; profile != nil {
		if opened.ActivePermissionProfile == nil || opened.ActivePermissionProfile.ID != profile.ID {
			return "", fmt.Errorf("codex activated an unexpected permission profile")
		}
		if len(opened.RuntimeWorkspaceRoots) != 1 || !sameExistingPath(opened.RuntimeWorkspaceRoots[0], d.config.Workspace) {
			return "", fmt.Errorf("codex activated unexpected runtime workspace roots")
		}
	}
	return opened.Thread.ID, nil
}

func (d *ProcessDriver) validateTurn(turn runtime.Turn, sink runtime.EventSink) error {
	if d == nil || sink == nil {
		return fmt.Errorf("codex driver and event sink are required")
	}
	if err := d.validateConfig(); err != nil {
		return err
	}
	if strings.TrimSpace(turn.Prompt) == "" {
		return fmt.Errorf("codex turn prompt is required")
	}
	return nil
}

func (d *ProcessDriver) validateConfig() error {
	if d == nil {
		return fmt.Errorf("codex driver is required")
	}
	if strings.TrimSpace(d.config.Executable) == "" || strings.TrimSpace(d.config.Workspace) == "" {
		return fmt.Errorf("codex executable and workspace are required")
	}
	if strings.TrimSpace(d.config.Model) == "" || strings.TrimSpace(d.config.ModelProvider) == "" {
		return fmt.Errorf("codex model and model provider are required")
	}
	if err := d.config.SandboxPolicy.validate(d.config.Workspace); err != nil {
		return err
	}
	if d.config.SandboxPolicy.PermissionProfile != nil {
		if !filepath.IsAbs(d.config.Executable) || filepath.Clean(d.config.Executable) != d.config.Executable {
			return fmt.Errorf("codex permission profile executable must be an absolute normalized path")
		}
		if !filepath.IsAbs(d.config.CredentialReadProbe) || filepath.Clean(d.config.CredentialReadProbe) != d.config.CredentialReadProbe || filepath.Base(d.config.CredentialReadProbe) != "auth.json" {
			return fmt.Errorf("codex credential read probe must be an absolute normalized auth.json path")
		}
	} else if d.config.CredentialReadProbe != "" {
		return fmt.Errorf("codex external sandbox must not configure a credential read probe")
	}
	if d.config.MaxEventBytes <= 0 || d.config.MaxStderrBytes <= 0 || d.config.HandshakeTimeout <= 0 || d.config.ShutdownGrace <= 0 {
		return fmt.Errorf("codex event, stderr, handshake, and shutdown limits must be positive")
	}
	return nil
}

func (d *ProcessDriver) awaitResponse(ctx context.Context, client *protocolClient, id int, operation string) (json.RawMessage, []rpcMessage, error) {
	waitCtx, cancel := context.WithTimeout(ctx, d.config.HandshakeTimeout)
	defer cancel()
	result, pending, err := client.awaitResponse(waitCtx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("codex %s handshake: %w", operation, err)
	}
	return result, pending, nil
}

func (d *ProcessDriver) cancelTurn(client *protocolClient, threadID, turnID string) bool {
	cancelCtx, cancel := context.WithTimeout(context.Background(), d.config.ShutdownGrace)
	defer cancel()
	if err := client.write(cancelCtx, request(interruptRequestID, "turn/interrupt", map[string]string{
		"threadId": threadID, "turnId": turnID,
	})); err != nil {
		return false
	}
	interruptAcknowledged := false
	targetCompleted := false
	for {
		message, err := client.next(cancelCtx)
		if err != nil {
			return false
		}
		if message.Method == "" {
			if string(message.ID) == fmt.Sprintf("%d", interruptRequestID) {
				if message.Error != nil {
					return false
				}
				interruptAcknowledged = true
			}
		} else if len(message.ID) != 0 {
			if err := client.answerServerRequest(cancelCtx, message); err != nil {
				return false
			}
		} else if message.Method == turnCompletedMethod {
			var params struct {
				ThreadID string `json:"threadId"`
				Turn     struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"turn"`
			}
			if json.Unmarshal(message.Params, &params) == nil && params.ThreadID == threadID && params.Turn.ID == turnID {
				switch params.Turn.Status {
				case "completed", "failed", "interrupted":
					targetCompleted = true
				}
			}
		}
		if interruptAcknowledged && targetCompleted {
			return true
		}
	}
}

func (d *ProcessDriver) terminate(cmd *exec.Cmd, waiter *processWaiter) {
	_ = interruptProcessGroup(cmd.Process)
	waiter.start()
	timer := time.NewTimer(d.config.ShutdownGrace)
	defer timer.Stop()
	select {
	case <-waiter.done:
		_ = killProcessGroup(cmd.Process)
	case <-timer.C:
		_ = killProcessGroup(cmd.Process)
		<-waiter.done
	}
}

func (d *ProcessDriver) shutdown(cmd *exec.Cmd, waiter *processWaiter) {
	waiter.start()
	timer := time.NewTimer(d.config.ShutdownGrace)
	defer timer.Stop()
	select {
	case <-waiter.done:
		return
	case <-timer.C:
	}
	// app-server normally exits on protocol EOF. Interrupt only as a bounded
	// fallback, then use the existing process-group cleanup for descendants.
	d.terminate(cmd, waiter)
}

type protocolClient struct {
	input           io.WriteCloser
	messages        <-chan readResult
	waiter          *processWaiter
	maxRequestBytes int
	maxPendingBytes int
	writeTimeout    time.Duration
}

const (
	maxPendingHandshakeMessages = 64
	maxProtocolRequestBytes     = 1 << 20
)

func (d *ProcessDriver) newProtocolClient(input io.WriteCloser, messages <-chan readResult, waiter *processWaiter) protocolClient {
	return protocolClient{
		input: input, messages: messages, waiter: waiter,
		maxRequestBytes: maxProtocolRequestBytes, maxPendingBytes: d.config.MaxEventBytes,
		writeTimeout: d.config.HandshakeTimeout,
	}
}

func (c *protocolClient) write(ctx context.Context, message any) error {
	writeCtx, cancel := context.WithTimeout(ctx, c.writeTimeout)
	defer cancel()
	return writeJSONLContext(writeCtx, c.input, c.maxRequestBytes, message)
}

func (c *protocolClient) awaitResponse(ctx context.Context, id int) (json.RawMessage, []rpcMessage, error) {
	pending := make([]rpcMessage, 0)
	pendingBytes := 0
	wantID := fmt.Sprintf("%d", id)
	for {
		message, err := c.next(ctx)
		if err != nil {
			return nil, nil, err
		}
		if message.Method != "" {
			if len(message.ID) != 0 {
				if err := c.answerServerRequest(ctx, message); err != nil {
					return nil, nil, err
				}
			} else {
				messageBytes := rpcMessageBytes(message)
				if len(pending) >= maxPendingHandshakeMessages || messageBytes > c.maxPendingBytes-pendingBytes {
					return nil, nil, fmt.Errorf("codex handshake emitted too many pending notifications")
				}
				pending = append(pending, message)
				pendingBytes += messageBytes
			}
			continue
		}
		if string(message.ID) != wantID {
			return nil, nil, fmt.Errorf("unexpected Codex response id %s while waiting for %s", message.ID, wantID)
		}
		if message.Error != nil {
			return nil, nil, fmt.Errorf("codex app-server request failed with code %d", message.Error.Code)
		}
		return message.Result, pending, nil
	}
}

func rpcMessageBytes(message rpcMessage) int {
	size := len(message.ID) + len(message.Method) + len(message.Params) + len(message.Result)
	if message.Error != nil {
		size += len(message.Error.Message) + len(message.Error.Data) + 32
	}
	return size
}

func (c *protocolClient) next(ctx context.Context) (rpcMessage, error) {
	select {
	case <-ctx.Done():
		return rpcMessage{}, ctx.Err()
	case result, ok := <-c.messages:
		if !ok {
			return rpcMessage{}, c.processExitError(io.EOF)
		}
		if result.err != nil {
			return rpcMessage{}, c.processExitError(result.err)
		}
		return result.message, nil
	}
}

func (c *protocolClient) processExitError(readErr error) error {
	waitErr, exited := c.waiter.result()
	if exited && waitErr != nil {
		// App-server stderr is untrusted vendor/runtime output and may contain
		// prompt fragments, paths, or credentials. Keep it bounded for future
		// explicitly reviewed diagnostics, but never reflect it to A2A callers.
		return fmt.Errorf("codex app-server exited: %w", waitErr)
	}
	if !exited {
		return fmt.Errorf("codex app-server protocol stream failed while process remained active: %w", readErr)
	}
	return fmt.Errorf("codex app-server closed its protocol stream: %w", readErr)
}

func (p SandboxPolicy) validate(workspace string) error {
	configured := 0
	if p.ExternalSandbox != nil {
		configured++
	}
	if p.PermissionProfile != nil {
		configured++
	}
	if configured != 1 {
		return fmt.Errorf("codex sandbox policy must configure exactly one of externalSandbox or permissionProfile")
	}
	if p.ExternalSandbox != nil {
		switch p.ExternalSandbox.NetworkAccess {
		case "restricted", "enabled":
			return nil
		default:
			return fmt.Errorf("codex externalSandbox network access must be restricted or enabled")
		}
	}
	if !filepath.IsAbs(workspace) {
		return fmt.Errorf("codex permission profile workspace must be an absolute path")
	}
	if strings.TrimSpace(p.PermissionProfile.ID) == "" || strings.TrimSpace(p.PermissionProfile.ID) != p.PermissionProfile.ID {
		return fmt.Errorf("codex permission profile id must be non-empty and normalized")
	}
	return nil
}

func (p SandboxPolicy) appServerPolicy() appServerSandboxPolicy {
	if p.ExternalSandbox != nil {
		return externalSandboxPolicy{
			Type:          "externalSandbox",
			NetworkAccess: p.ExternalSandbox.NetworkAccess,
		}
	}
	return nil
}

func (c *protocolClient) answerServerRequest(ctx context.Context, message rpcMessage) error {
	switch message.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		return c.write(ctx, response(message.ID, map[string]string{"decision": "cancel"}))
	case "applyPatchApproval", "execCommandApproval":
		return c.write(ctx, response(message.ID, map[string]string{"decision": "abort"}))
	case "item/permissions/requestApproval":
		return c.write(ctx, errorResponse(message.ID, -32001, "permission approvals are not supported"))
	default:
		return c.write(ctx, errorResponse(message.ID, -32601, "unsupported server request"))
	}
}

type threadStartParams struct {
	Model                 string   `json:"model,omitempty"`
	ModelProvider         string   `json:"modelProvider,omitempty"`
	CWD                   string   `json:"cwd"`
	DeveloperInstructions string   `json:"developerInstructions,omitempty"`
	ApprovalPolicy        string   `json:"approvalPolicy,omitempty"`
	ServiceTier           string   `json:"serviceTier,omitempty"`
	Permissions           string   `json:"permissions,omitempty"`
	RuntimeWorkspaceRoots []string `json:"runtimeWorkspaceRoots,omitempty"`
}

type threadResumeParams struct {
	ThreadID              string   `json:"threadId"`
	Model                 string   `json:"model,omitempty"`
	ModelProvider         string   `json:"modelProvider,omitempty"`
	CWD                   string   `json:"cwd"`
	DeveloperInstructions string   `json:"developerInstructions,omitempty"`
	ApprovalPolicy        string   `json:"approvalPolicy,omitempty"`
	ServiceTier           string   `json:"serviceTier,omitempty"`
	Permissions           string   `json:"permissions,omitempty"`
	RuntimeWorkspaceRoots []string `json:"runtimeWorkspaceRoots,omitempty"`
}

type turnStartParams struct {
	ThreadID       string                 `json:"threadId"`
	Input          []userInput            `json:"input"`
	Model          *string                `json:"model,omitempty"`
	Effort         *string                `json:"effort,omitempty"`
	ServiceTier    *string                `json:"serviceTier,omitempty"`
	ApprovalPolicy string                 `json:"approvalPolicy"`
	SandboxPolicy  appServerSandboxPolicy `json:"sandboxPolicy,omitempty"`
}

type appServerSandboxPolicy interface {
	appServerSandboxPolicy()
}

type externalSandboxPolicy struct {
	Type          string `json:"type"`
	NetworkAccess string `json:"networkAccess"`
}

func (externalSandboxPolicy) appServerSandboxPolicy() {}

type userInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func sameExistingPath(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

func fileSHA256(path string) ([sha256.Size]byte, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	digest := sha256.Sum256(contents)
	clear(contents)
	return digest, nil
}

type boundedBuffer struct {
	bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.max - b.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return original, nil
}

type processWaiter struct {
	cmd  *exec.Cmd
	once sync.Once
	done chan struct{}
	err  error
}

func newProcessWaiter(cmd *exec.Cmd) *processWaiter {
	return &processWaiter{cmd: cmd, done: make(chan struct{})}
}

func (w *processWaiter) start() {
	w.once.Do(func() {
		go func() {
			w.err = w.cmd.Wait()
			close(w.done)
		}()
	})
}

func (w *processWaiter) result() (error, bool) {
	w.start()
	select {
	case <-w.done:
		return w.err, true
	default:
		return nil, false
	}
}
