// Package session persists the one native conversation owned by a Harness
// runtime Actor.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
)

const stateVersion = 2

type state struct {
	Version   int    `json:"version"`
	Runtime   string `json:"runtime"`
	SessionID string `json:"session_id,omitempty"`
}

// Store binds an Actor to exactly one native runtime conversation.
type Store struct {
	mu      sync.RWMutex
	path    string
	runtime string
	data    state
}

// New opens the session store for one named Harness runtime.
func New(durableDir, runtimeName string) (*Store, error) {
	runtimeName = strings.TrimSpace(runtimeName)
	if runtimeName == "" {
		return nil, fmt.Errorf("runtime name is required")
	}
	if err := os.MkdirAll(durableDir, 0o700); err != nil {
		return nil, fmt.Errorf("create session state directory: %w", err)
	}
	if err := os.Chmod(durableDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure session state directory: %w", err)
	}
	s := &Store{
		path:    filepath.Join(durableDir, "state.json"),
		runtime: runtimeName,
		data:    state{Version: stateVersion, Runtime: runtimeName},
	}
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read session state: %w", err)
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, fmt.Errorf("decode session state: %w", err)
	}
	if s.data.Version != stateVersion || s.data.Runtime != runtimeName {
		return nil, fmt.Errorf("unsupported or corrupt %s session state", runtimeName)
	}
	if s.data.SessionID != "" {
		if err := validateSessionID(s.data.SessionID); err != nil {
			return nil, fmt.Errorf("invalid persisted session state: %w", err)
		}
	}
	return s, nil
}

// Load returns the bound native conversation, when one exists.
func (s *Store) Load() (string, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.SessionID, s.data.SessionID != "", nil
}

// Bind persists the first native conversation ID and rejects later conflicts.
func (s *Store) Bind(nativeSessionID string) error {
	if err := validateSessionID(nativeSessionID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.SessionID != "" && s.data.SessionID != nativeSessionID {
		return fmt.Errorf("actor is already bound to another %s session", s.runtime)
	}
	if s.data.SessionID == nativeSessionID {
		return nil
	}
	next := state{Version: stateVersion, Runtime: s.runtime, SessionID: nativeSessionID}
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".sessions-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary session state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure temporary session state: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary session state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary session state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary session state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace session state: %w", err)
	}
	s.data = next
	return nil
}

func validateSessionID(nativeSessionID string) error {
	if _, err := uuid.Parse(nativeSessionID); err != nil {
		return fmt.Errorf("invalid native session ID: %w", err)
	}
	return nil
}
