package driver

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kagent-dev/kagent/go/harness/runtime"
)

const (
	agentMessageDeltaMethod = "item/agentMessage/delta"
	itemStartedMethod       = "item/started"
	itemCompletedMethod     = "item/completed"
	turnCompletedMethod     = "turn/completed"
	maxAgentMessageBytes    = 1 << 20
)

type turnEvents struct {
	threadID     string
	turnID       string
	sink         runtime.EventSink
	tools        map[string]string
	messages     map[string]*strings.Builder
	messageBytes int
	// maxMessageBytes bounds all in-flight text across any number of individually
	// valid delta frames and message IDs. Tests override it to exercise the
	// boundary cheaply.
	maxMessageBytes int
}

type threadItem struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	Command          string          `json:"command"`
	CWD              string          `json:"cwd"`
	Status           string          `json:"status"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	AggregatedOutput *string         `json:"aggregatedOutput"`
	ExitCode         *int            `json:"exitCode"`
	Result           json.RawMessage `json:"result"`
	Error            json.RawMessage `json:"error"`
	Changes          json.RawMessage `json:"changes"`
	ContentItems     json.RawMessage `json:"contentItems"`
	Success          *bool           `json:"success"`
	Text             string          `json:"text"`
}

func newTurnEvents(threadID, turnID string, sink runtime.EventSink) *turnEvents {
	return &turnEvents{
		threadID: threadID, turnID: turnID, sink: sink,
		tools: map[string]string{}, messages: map[string]*strings.Builder{},
		maxMessageBytes: maxAgentMessageBytes,
	}
}

func (t *turnEvents) handle(message rpcMessage) (*runtime.Outcome, bool, error) {
	switch message.Method {
	case agentMessageDeltaMethod:
		var params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
		}
		if err := decodeParams(message.Params, &params); err != nil {
			return nil, false, err
		}
		if err := t.requireTurn(params.ThreadID, params.TurnID); err != nil {
			return nil, false, err
		}
		if params.ItemID == "" {
			return nil, false, fmt.Errorf("codex agent message delta omitted item id")
		}
		if params.Delta != "" {
			message := t.messages[params.ItemID]
			if message == nil {
				message = &strings.Builder{}
			}
			if len(params.Delta) > t.maxMessageBytes-t.messageBytes {
				return nil, false, fmt.Errorf("codex agent message %q exceeded %d bytes", params.ItemID, t.maxMessageBytes)
			}
			if err := t.sink.TextDelta(runtime.TextDelta{Text: params.Delta}); err != nil {
				return nil, false, err
			}
			message.WriteString(params.Delta)
			t.messageBytes += len(params.Delta)
			t.messages[params.ItemID] = message
		}
	case itemStartedMethod:
		item, err := t.decodeItem(message.Params)
		if err != nil {
			return nil, false, err
		}
		if err := rejectUnsupportedItem(item); err != nil {
			return nil, false, err
		}
		if err := t.startTool(item); err != nil {
			return nil, false, err
		}
	case itemCompletedMethod:
		item, err := t.decodeItem(message.Params)
		if err != nil {
			return nil, false, err
		}
		if err := rejectUnsupportedItem(item); err != nil {
			return nil, false, err
		}
		if err := t.completeItem(item); err != nil {
			return nil, false, err
		}
	case turnCompletedMethod:
		var params struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Error  *struct {
					Message        string          `json:"message"`
					CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
				} `json:"error"`
			} `json:"turn"`
		}
		if err := decodeParams(message.Params, &params); err != nil {
			return nil, false, err
		}
		if err := t.requireTurn(params.ThreadID, params.Turn.ID); err != nil {
			return nil, false, err
		}
		if params.Turn.Status == "completed" {
			if len(t.tools) != 0 || len(t.messages) != 0 {
				return nil, false, fmt.Errorf("codex completed a turn with unfinished items")
			}
			return &runtime.Outcome{}, true, nil
		}
		failure := safeTurnFailure(params.Turn.Status, params.Turn.Error)
		return &runtime.Outcome{Failure: &runtime.Failure{Message: failure}}, true, nil
	}
	return nil, false, nil
}

func rejectUnsupportedItem(item threadItem) error {
	switch item.Type {
	case "collabAgentToolCall", "subAgentActivity":
		// Do not include the item ID or vendor payload: either can contain model-
		// generated or credential-shaped data that must not reach A2A callers.
		return fmt.Errorf("codex emitted unsupported multi-agent item type %q", item.Type)
	default:
		return nil
	}
}

func (t *turnEvents) decodeItem(raw json.RawMessage) (threadItem, error) {
	var params struct {
		ThreadID string          `json:"threadId"`
		TurnID   string          `json:"turnId"`
		Item     json.RawMessage `json:"item"`
	}
	if err := decodeParams(raw, &params); err != nil {
		return threadItem{}, err
	}
	if err := t.requireTurn(params.ThreadID, params.TurnID); err != nil {
		return threadItem{}, err
	}
	var item threadItem
	if err := json.Unmarshal(params.Item, &item); err != nil {
		return threadItem{}, fmt.Errorf("decode Codex thread item: %w", err)
	}
	if item.ID == "" || item.Type == "" {
		return threadItem{}, fmt.Errorf("codex thread item requires id and type")
	}
	return item, nil
}

func (t *turnEvents) startTool(item threadItem) error {
	name, arguments, ok, err := toolStart(item)
	if err != nil || !ok {
		return err
	}
	if previous := t.tools[item.ID]; previous != "" {
		return fmt.Errorf("codex tool item %q started more than once", item.ID)
	}
	t.tools[item.ID] = name
	return t.sink.ToolCall(runtime.ToolCall{ID: item.ID, Name: name, Arguments: arguments})
}

func (t *turnEvents) completeItem(item threadItem) error {
	if item.Type == "agentMessage" {
		return t.completeAgentMessage(item)
	}
	name, result, isError, ok, err := toolCompletion(item)
	if err != nil || !ok {
		return err
	}
	startedName := t.tools[item.ID]
	if startedName == "" {
		return fmt.Errorf("codex tool item %q completed before it started", item.ID)
	}
	if startedName != name {
		return fmt.Errorf("codex tool item %q changed name from %q to %q", item.ID, startedName, name)
	}
	delete(t.tools, item.ID)
	return t.sink.ToolResult(runtime.ToolResult{ID: item.ID, Name: name, Result: result, IsError: isError})
}

func (t *turnEvents) completeAgentMessage(item threadItem) error {
	if len(item.Text) > t.maxMessageBytes {
		return fmt.Errorf("codex agent message %q exceeded %d bytes", item.ID, t.maxMessageBytes)
	}
	streamed := ""
	if message := t.messages[item.ID]; message != nil {
		streamed = message.String()
	}
	if streamed == item.Text {
		t.deleteMessage(item.ID)
		return nil
	}
	if remaining, ok := strings.CutPrefix(item.Text, streamed); ok {
		if remaining == "" {
			t.deleteMessage(item.ID)
			return nil
		}
		if err := t.sink.TextDelta(runtime.TextDelta{Text: remaining}); err != nil {
			return err
		}
		t.deleteMessage(item.ID)
		return nil
	}
	return fmt.Errorf("codex completed agent message %q with text inconsistent with streamed deltas", item.ID)
}

func (t *turnEvents) deleteMessage(itemID string) {
	if message := t.messages[itemID]; message != nil {
		t.messageBytes -= message.Len()
		delete(t.messages, itemID)
	}
}

func safeTurnFailure(status string, failure *struct {
	Message        string          `json:"message"`
	CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
}) string {
	if status == "interrupted" {
		return "Codex execution was interrupted"
	}
	kind := codexErrorKind(nil)
	if failure != nil {
		kind = codexErrorKind(failure.CodexErrorInfo)
	}
	switch kind {
	case "contextWindowExceeded":
		return "Codex context window was exceeded"
	case "sessionBudgetExceeded", "usageLimitExceeded":
		return "Codex usage limit was exceeded"
	case "serverOverloaded":
		return "Codex service is overloaded"
	case "cyberPolicy", "misalignmentPolicyViolation":
		return "Codex policy blocked the request"
	case "unauthorized":
		return "Codex authentication failed"
	case "badRequest":
		return "Codex rejected the request"
	case "sandboxError":
		return "Codex sandbox execution failed"
	case "httpConnectionFailed", "responseStreamConnectionFailed", "responseStreamDisconnected", "responseTooManyFailedAttempts":
		return "Codex service connection failed"
	default:
		return "Codex execution failed"
	}
}

func codexErrorKind(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var literal string
	if json.Unmarshal(raw, &literal) == nil {
		return literal
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) != 1 {
		return ""
	}
	for kind := range object {
		return kind
	}
	return ""
}

func toolStart(item threadItem) (string, map[string]any, bool, error) {
	switch item.Type {
	case "commandExecution":
		if item.Command == "" {
			return "", nil, false, fmt.Errorf("codex command item %q requires a command", item.ID)
		}
		return "shell", map[string]any{"command": item.Command, "cwd": item.CWD}, true, nil
	case "mcpToolCall":
		if item.Server == "" || item.Tool == "" {
			return "", nil, false, fmt.Errorf("codex MCP item %q requires server and tool", item.ID)
		}
		arguments, err := objectOrValue(item.Arguments)
		return item.Server + "/" + item.Tool, arguments, true, err
	case "dynamicToolCall":
		if item.Tool == "" {
			return "", nil, false, fmt.Errorf("codex dynamic tool item %q requires a tool", item.ID)
		}
		arguments, err := objectOrValue(item.Arguments)
		return item.Tool, arguments, true, err
	case "fileChange":
		changes, err := decodeAny(item.Changes)
		return "apply_patch", map[string]any{"changes": changes}, true, err
	default:
		return "", nil, false, nil
	}
}

func toolCompletion(item threadItem) (string, any, bool, bool, error) {
	switch item.Type {
	case "commandExecution":
		result := map[string]any{"status": item.Status, "output": item.AggregatedOutput, "exitCode": item.ExitCode}
		return "shell", result, item.Status != "completed" || (item.ExitCode != nil && *item.ExitCode != 0), true, nil
	case "mcpToolCall":
		result, err := decodeAny(item.Result)
		if err != nil {
			return "", nil, false, false, err
		}
		if len(item.Error) != 0 && string(item.Error) != "null" {
			result, err = decodeAny(item.Error)
			if err != nil {
				return "", nil, false, false, err
			}
		}
		return item.Server + "/" + item.Tool, result, item.Status == "failed", true, nil
	case "dynamicToolCall":
		result, err := decodeAny(item.ContentItems)
		isError := item.Status == "failed" || (item.Success != nil && !*item.Success)
		return item.Tool, result, isError, true, err
	case "fileChange":
		result, err := decodeAny(item.Changes)
		return "apply_patch", result, item.Status != "completed", true, err
	default:
		return "", nil, false, false, nil
	}
}

func objectOrValue(raw json.RawMessage) (map[string]any, error) {
	value, err := decodeAny(raw)
	if err != nil {
		return nil, err
	}
	if object, ok := value.(map[string]any); ok {
		return object, nil
	}
	return map[string]any{"value": value}, nil
}

func decodeAny(raw json.RawMessage) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("decode Codex item value: %w", err)
	}
	return value, nil
}

func decodeParams(raw json.RawMessage, target any) error {
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode Codex notification parameters: %w", err)
	}
	return nil
}

func (t *turnEvents) requireTurn(threadID, turnID string) error {
	if threadID != t.threadID || turnID != t.turnID {
		return fmt.Errorf("codex event belongs to thread %q turn %q, want thread %q turn %q", threadID, turnID, t.threadID, t.turnID)
	}
	return nil
}
