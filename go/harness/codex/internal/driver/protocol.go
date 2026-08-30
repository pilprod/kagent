package driver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type readResult struct {
	message rpcMessage
	err     error
}

func readJSONL(r io.Reader, maxEventBytes int, stop <-chan struct{}) <-chan readResult {
	results := make(chan readResult, 32)
	go func() {
		defer close(results)
		reader := bufio.NewReaderSize(r, min(maxEventBytes+1, 64*1024))
		for {
			line, err := readBoundedLine(reader, maxEventBytes)
			if len(bytes.TrimSpace(line)) != 0 {
				var message rpcMessage
				if decodeErr := json.Unmarshal(line, &message); decodeErr != nil {
					emitReadResult(results, stop, readResult{err: fmt.Errorf("decode Codex app-server message: %w", decodeErr)})
					return
				}
				if len(message.ID) == 0 && message.Method == "" {
					emitReadResult(results, stop, readResult{err: fmt.Errorf("codex app-server message has neither id nor method")})
					return
				}
				if !emitReadResult(results, stop, readResult{message: message}) {
					return
				}
			}
			if err == io.EOF {
				emitReadResult(results, stop, readResult{err: io.EOF})
				return
			}
			if err != nil {
				emitReadResult(results, stop, readResult{err: fmt.Errorf("read Codex app-server message: %w", err)})
				return
			}
		}
	}()
	return results
}

func emitReadResult(results chan<- readResult, stop <-chan struct{}, result readResult) bool {
	select {
	case results <- result:
		return true
	case <-stop:
		return false
	}
}

func readBoundedLine(r *bufio.Reader, max int) ([]byte, error) {
	if max <= 0 {
		return nil, fmt.Errorf("maximum Codex event size must be positive")
	}
	var line []byte
	for {
		fragment, err := r.ReadSlice('\n')
		if len(line)+len(fragment) > max {
			return nil, fmt.Errorf("codex app-server message exceeds %d bytes", max)
		}
		line = append(line, fragment...)
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

func writeJSONL(w io.Writer, message any) error {
	raw, err := encodeJSONL(message, 0)
	if err != nil {
		return err
	}
	if err := writeProtocolBytes(w, raw); err != nil {
		return fmt.Errorf("write Codex app-server message: %w", err)
	}
	return nil
}

// writeJSONLContext bounds both the encoded request and a potentially blocked
// app-server stdin write. Closing the pipe on cancellation releases the write
// and forces the owning driver down its process-tree cleanup path.
func writeJSONLContext(ctx context.Context, w io.WriteCloser, maxBytes int, message any) error {
	if ctx == nil || w == nil || maxBytes <= 0 {
		return fmt.Errorf("bounded Codex app-server writer is not configured")
	}
	raw, err := encodeJSONL(message, maxBytes)
	if err != nil {
		return err
	}
	result := make(chan error, 1)
	go func() {
		err := writeProtocolBytes(w, raw)
		clear(raw)
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			return fmt.Errorf("write Codex app-server message: %w", err)
		}
		return nil
	case <-ctx.Done():
		_ = w.Close()
		return fmt.Errorf("write Codex app-server message: %w", ctx.Err())
	}
}

func encodeJSONL(message any, maxBytes int) ([]byte, error) {
	raw, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("encode Codex app-server message: %w", err)
	}
	raw = append(raw, '\n')
	if maxBytes > 0 && len(raw) > maxBytes {
		clear(raw)
		return nil, fmt.Errorf("Codex app-server request exceeds %d bytes", maxBytes)
	}
	return raw, nil
}

func writeProtocolBytes(w io.Writer, raw []byte) error {
	for written := 0; written < len(raw); {
		count, err := w.Write(raw[written:])
		if count < 0 || count > len(raw)-written || count == 0 {
			return io.ErrShortWrite
		}
		written += count
		if err != nil {
			return err
		}
	}
	return nil
}

func request(id int, method string, params any) map[string]any {
	return map[string]any{"id": id, "method": method, "params": params}
}

func notification(method string, params any) map[string]any {
	return map[string]any{"method": method, "params": params}
}

func response(id json.RawMessage, result any) map[string]any {
	return map[string]any{"id": id, "result": result}
}

func errorResponse(id json.RawMessage, code int, message string) map[string]any {
	return map[string]any{"id": id, "error": map[string]any{"code": code, "message": message}}
}
