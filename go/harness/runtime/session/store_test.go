package session_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/harness/runtime/session"
)

const sessionID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

func TestStoreFirstSessionReloadAndPermissions(t *testing.T) {
	dir := t.TempDir()
	store, err := session.New(dir, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Load(); err != nil || ok {
		t.Fatalf("empty Load() = ok %t, err %v", ok, err)
	}
	if err := store.Bind(sessionID); err != nil {
		t.Fatal(err)
	}
	reloaded, err := session.New(dir, "codex")
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := reloaded.Load()
	if err != nil || !ok || got != sessionID {
		t.Fatalf("reloaded Load() = %q, %t, %v", got, ok, err)
	}
	info, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state permissions = %o, want 600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || strings.Contains(entries[0].Name(), ".tmp") {
		t.Errorf("atomic replacement left entries: %v", entries)
	}
}

func TestStoreRejectsCorruptUnsupportedAndOtherRuntimeState(t *testing.T) {
	tests := []string{
		"not json",
		`{"version":1,"runtime":"codex","session_id":"` + sessionID + `"}`,
		`{"version":2,"runtime":"claude","session_id":"` + sessionID + `"}`,
		`{"version":2,"runtime":"codex","session_id":"not-a-session"}`,
	}
	for _, contents := range tests {
		t.Run(contents, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := session.New(dir, "codex"); err == nil {
				t.Fatal("New() succeeded for invalid state")
			}
		})
	}
}

func TestStoreRejectsInvalidRuntimeAndConflictingSessionIDs(t *testing.T) {
	if _, err := session.New(t.TempDir(), " "); err == nil {
		t.Fatal("New() accepted an empty runtime name")
	}
	store, err := session.New(t.TempDir(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Bind("not-a-session"); err == nil {
		t.Fatal("Bind() accepted invalid session ID")
	}
	if err := store.Bind(sessionID); err != nil {
		t.Fatal(err)
	}
	if err := store.Bind(sessionID); err != nil {
		t.Fatalf("idempotent Bind() failed: %v", err)
	}
	if err := store.Bind("cccccccc-cccc-4ccc-8ccc-cccccccccccc"); err == nil {
		t.Fatal("Bind() accepted a conflicting native session")
	}
}
