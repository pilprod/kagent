package driver

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/harness/runtime"
)

func TestTurnEventsStreamsTextToolsAndCompletion(t *testing.T) {
	sink := &recordingSink{}
	events := newTurnEvents("thread-1", "turn-1", sink)
	messages := []rpcMessage{
		notify(t, agentMessageDeltaMethod, map[string]any{"threadId": "thread-1", "turnId": "turn-1", "itemId": "message-1", "delta": "hello"}),
		notify(t, itemCompletedMethod, map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{
			"type": "agentMessage", "id": "message-1", "text": "hello",
		}}),
		notify(t, itemStartedMethod, map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{
			"type": "commandExecution", "id": "tool-1", "command": "go test ./...", "cwd": "/workspace", "status": "inProgress",
		}}),
		notify(t, itemCompletedMethod, map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{
			"type": "commandExecution", "id": "tool-1", "command": "go test ./...", "cwd": "/workspace", "status": "completed", "aggregatedOutput": "ok", "exitCode": 0,
		}}),
		notify(t, turnCompletedMethod, map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "completed", "items": []any{}}}),
	}
	var terminal bool
	for _, message := range messages {
		outcome, done, err := events.handle(message)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			terminal = true
			if outcome == nil || outcome.Failure != nil {
				t.Fatalf("terminal outcome = %#v", outcome)
			}
		}
	}
	if !terminal {
		t.Fatal("turn did not complete")
	}
	if len(sink.deltas) != 1 || sink.deltas[0].Text != "hello" {
		t.Fatalf("text deltas = %#v", sink.deltas)
	}
	if len(sink.calls) != 1 || sink.calls[0].Name != "shell" || sink.calls[0].Arguments["command"] != "go test ./..." {
		t.Fatalf("tool calls = %#v", sink.calls)
	}
	if len(sink.results) != 1 || sink.results[0].Name != "shell" || sink.results[0].IsError {
		t.Fatalf("tool results = %#v", sink.results)
	}
}

func TestTurnEventsMapsMCPFailure(t *testing.T) {
	sink := &recordingSink{}
	events := newTurnEvents("thread-1", "turn-1", sink)
	started := notify(t, itemStartedMethod, map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{
		"type": "mcpToolCall", "id": "mcp-1", "server": "tools", "tool": "lookup", "arguments": map[string]any{"id": "42"}, "status": "inProgress",
	}})
	completed := notify(t, itemCompletedMethod, map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{
		"type": "mcpToolCall", "id": "mcp-1", "server": "tools", "tool": "lookup", "arguments": map[string]any{"id": "42"}, "status": "failed", "error": map[string]any{"message": "denied"},
	}})
	if _, _, err := events.handle(started); err != nil {
		t.Fatal(err)
	}
	if _, _, err := events.handle(completed); err != nil {
		t.Fatal(err)
	}
	if len(sink.results) != 1 || !sink.results[0].IsError || sink.results[0].Name != "tools/lookup" {
		t.Fatalf("MCP result = %#v", sink.results)
	}
}

func TestTurnEventsRejectsCrossTurnAndUnpairedCompletion(t *testing.T) {
	for name, message := range map[string]rpcMessage{
		"cross turn": notify(t, agentMessageDeltaMethod, map[string]any{"threadId": "thread-2", "turnId": "turn-1", "delta": "x"}),
		"unpaired completion": notify(t, itemCompletedMethod, map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{
			"type": "fileChange", "id": "patch-1", "status": "completed", "changes": []any{},
		}}),
	} {
		t.Run(name, func(t *testing.T) {
			events := newTurnEvents("thread-1", "turn-1", &recordingSink{})
			if _, _, err := events.handle(message); err == nil {
				t.Fatal("event was accepted")
			}
		})
	}
}

func TestTurnEventsReconcilesCompletedAgentMessage(t *testing.T) {
	tests := []struct {
		name      string
		deltas    []string
		final     string
		want      []string
		wantError bool
	}{
		{name: "full text without deltas", final: "complete", want: []string{"complete"}},
		{name: "only missing suffix", deltas: []string{"hel", "lo"}, final: "hello world", want: []string{"hel", "lo", " world"}},
		{name: "exact final", deltas: []string{"done"}, final: "done", want: []string{"done"}},
		{name: "inconsistent final", deltas: []string{"old"}, final: "new", want: []string{"old"}, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &recordingSink{}
			events := newTurnEvents("thread-1", "turn-1", sink)
			for _, delta := range tt.deltas {
				_, _, err := events.handle(notify(t, agentMessageDeltaMethod, map[string]any{
					"threadId": "thread-1", "turnId": "turn-1", "itemId": "message-1", "delta": delta,
				}))
				if err != nil {
					t.Fatal(err)
				}
			}
			_, _, err := events.handle(notify(t, itemCompletedMethod, map[string]any{
				"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{
					"type": "agentMessage", "id": "message-1", "text": tt.final,
				},
			}))
			if (err != nil) != tt.wantError {
				t.Fatalf("handle() error = %v, wantError %t", err, tt.wantError)
			}
			got := make([]string, 0, len(sink.deltas))
			for _, delta := range sink.deltas {
				got = append(got, delta.Text)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("deltas = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTurnEventsBoundsAggregateAgentMessage(t *testing.T) {
	sink := &recordingSink{}
	events := newTurnEvents("thread-1", "turn-1", sink)
	events.maxMessageBytes = 5

	for _, delta := range []string{"he", "llo"} {
		if _, _, err := events.handle(notify(t, agentMessageDeltaMethod, map[string]any{
			"threadId": "thread-1", "turnId": "turn-1", "itemId": "message-1", "delta": delta,
		})); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := events.handle(notify(t, agentMessageDeltaMethod, map[string]any{
		"threadId": "thread-1", "turnId": "turn-1", "itemId": "message-1", "delta": "!",
	})); err == nil || !strings.Contains(err.Error(), "exceeded 5 bytes") {
		t.Fatalf("oversized aggregate error = %v", err)
	}
	if got := len(sink.deltas); got != 2 {
		t.Fatalf("emitted deltas = %d, want 2", got)
	}
	if _, _, err := events.handle(notify(t, agentMessageDeltaMethod, map[string]any{
		"threadId": "thread-1", "turnId": "turn-1", "itemId": "message-2", "delta": "x",
	})); err == nil || !strings.Contains(err.Error(), "exceeded 5 bytes") {
		t.Fatalf("second in-flight message error = %v", err)
	}

	finalOnly := newTurnEvents("thread-1", "turn-1", &recordingSink{})
	finalOnly.maxMessageBytes = 5
	if _, _, err := finalOnly.handle(notify(t, itemCompletedMethod, map[string]any{
		"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{
			"type": "agentMessage", "id": "message-2", "text": "123456",
		},
	})); err == nil || !strings.Contains(err.Error(), "exceeded 5 bytes") {
		t.Fatalf("oversized completed message error = %v", err)
	}
}

func TestTurnEventsRejectsMultiAgentItems(t *testing.T) {
	const sensitiveID = "credential-shaped-item-id"
	for _, method := range []string{itemStartedMethod, itemCompletedMethod} {
		for _, itemType := range []string{"collabAgentToolCall", "subAgentActivity"} {
			t.Run(method+"/"+itemType, func(t *testing.T) {
				events := newTurnEvents("thread-1", "turn-1", &recordingSink{})
				_, _, err := events.handle(notify(t, method, map[string]any{
					"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{
						"type": itemType, "id": sensitiveID, "text": "secret vendor payload",
					},
				}))
				if err == nil || !strings.Contains(err.Error(), "unsupported multi-agent item") {
					t.Fatalf("handle() error = %v", err)
				}
				if strings.Contains(err.Error(), sensitiveID) || strings.Contains(err.Error(), "secret vendor payload") {
					t.Fatalf("handle() reflected unsafe item data: %v", err)
				}
			})
		}
	}
}

func TestTurnEventsSanitizesVendorFailure(t *testing.T) {
	sink := &recordingSink{}
	events := newTurnEvents("thread-1", "turn-1", sink)
	outcome, terminal, err := events.handle(notify(t, turnCompletedMethod, map[string]any{
		"threadId": "thread-1", "turn": map[string]any{
			"id": "turn-1", "status": "failed", "items": []any{},
			"error": map[string]any{"message": "secret upstream diagnostic", "codexErrorInfo": "unauthorized"},
		},
	}))
	if err != nil || !terminal || outcome == nil || outcome.Failure == nil {
		t.Fatalf("terminal failure = %#v, %t, %v", outcome, terminal, err)
	}
	if outcome.Failure.Message != "Codex authentication failed" || strings.Contains(outcome.Failure.Message, "secret") {
		t.Fatalf("unsafe failure = %q", outcome.Failure.Message)
	}
}

func notify(t *testing.T, method string, params any) rpcMessage {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return rpcMessage{Method: method, Params: raw}
}

type recordingSink struct {
	sessions []runtime.SessionStarted
	deltas   []runtime.TextDelta
	calls    []runtime.ToolCall
	results  []runtime.ToolResult
}

func (s *recordingSink) SessionStarted(event runtime.SessionStarted) error {
	s.sessions = append(s.sessions, event)
	return nil
}

func (s *recordingSink) TextDelta(event runtime.TextDelta) error {
	s.deltas = append(s.deltas, event)
	return nil
}

func (s *recordingSink) ToolCall(event runtime.ToolCall) error {
	s.calls = append(s.calls, event)
	return nil
}

func (s *recordingSink) ToolResult(event runtime.ToolResult) error {
	s.results = append(s.results, event)
	return nil
}
