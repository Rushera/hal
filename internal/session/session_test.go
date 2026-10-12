package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Rushera/hal/internal/agent"
)

func TestSessionRoundTrip(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	s, err := New(root, cwd, "my task", "test-model")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(agent.NewUserMessage("hello")); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(agent.Message{Role: agent.RoleAssistant, Content: "hi there"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModel("other-model"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetName("renamed"); err != nil {
		t.Fatal(err)
	}
	path := s.Path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("session file not written: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != s.ID {
		t.Fatalf("id mismatch: %s vs %s", loaded.ID, s.ID)
	}
	if loaded.Name != "renamed" {
		t.Fatalf("name not persisted: %q", loaded.Name)
	}
	if loaded.Model != "other-model" {
		t.Fatalf("model not persisted: %q", loaded.Model)
	}
	if len(loaded.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(loaded.Messages))
	}
	if loaded.Messages[0].Content != "hello" || loaded.Messages[1].Content != "hi there" {
		t.Fatalf("messages not reconstructed: %+v", loaded.Messages)
	}

	if !strings.HasSuffix(filepath.Dir(path), ProjectDir("", cwd)[1:]) {
		// ProjectDir("", cwd) is relative; just check the bucket name appears
		slug := strings.TrimPrefix(ProjectDir("", cwd), "/")
		if !strings.Contains(path, slug) {
			t.Fatalf("session not stored in the project bucket: %s", path)
		}
	}
}

func TestLatestAndList(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	if _, err := Latest(root, cwd); err == nil {
		t.Fatal("expected ErrNoSessions for a fresh directory")
	}

	first, err := New(root, cwd, "first", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = first.AppendMessage(agent.NewUserMessage("one"))
	_ = first.Close()

	second, err := New(root, cwd, "second", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = second.AppendMessage(agent.NewUserMessage("two"))
	_ = second.Close()

	latest, err := Latest(root, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if latest != second.Path {
		t.Fatalf("Latest returned %s, want %s", latest, second.Path)
	}

	infos, err := List(root, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(infos))
	}
	if infos[0].Path != second.Path {
		t.Fatalf("sessions not sorted newest first: %+v", infos)
	}
	if infos[0].Messages != 1 {
		t.Fatalf("expected 1 message counted, got %d", infos[0].Messages)
	}
}

func TestSessionsAreGroupedPerDirectory(t *testing.T) {
	root := t.TempDir()
	a := t.TempDir()
	b := t.TempDir()

	if _, err := New(root, a, "in-a", "m"); err != nil {
		t.Fatal(err)
	}
	infosA, _ := List(root, a)
	infosB, _ := List(root, b)
	if len(infosA) != 1 {
		t.Fatalf("expected 1 session for dir a, got %d", len(infosA))
	}
	if len(infosB) != 0 {
		t.Fatalf("expected 0 sessions for dir b, got %d", len(infosB))
	}
}

// isolate sets HOME and the XDG variables so the default paths are predictable.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("HAL_SESSION_DIR", "")
	return home
}

func TestRootWithDefaultsToXDGDataDirectory(t *testing.T) {
	home := isolate(t)
	want := filepath.Join(home, "data", "hal", "sessions")
	if got := RootWith(""); got != want {
		t.Fatalf("RootWith(\"\") = %s, want %s", got, want)
	}
	if got := RootWith("/tmp/explicit"); got != "/tmp/explicit" {
		t.Fatalf("an explicit directory must win: %s", got)
	}
	t.Setenv("HAL_SESSION_DIR", "/tmp/from-env")
	if got := RootWith(""); got != "/tmp/from-env" {
		t.Fatalf("HAL_SESSION_DIR must win over XDG: %s", got)
	}
}

func TestSearchRootsIncludesLegacyOnlyWhenItExists(t *testing.T) {
	home := isolate(t)
	legacy := filepath.Join(home, ".hal", "sessions")

	roots := SearchRoots("")
	if len(roots) != 1 {
		t.Fatalf("expected only the canonical root, got %v", roots)
	}
	if want := filepath.Join(home, "data", "hal", "sessions"); roots[0] != want {
		t.Fatalf("canonical root = %s, want %s", roots[0], want)
	}

	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	roots = SearchRoots("")
	if len(roots) != 2 || roots[1] != legacy {
		t.Fatalf("legacy root should be searched when present, got %v", roots)
	}
	if roots[0] == roots[1] {
		t.Fatalf("canonical root must come first: %v", roots)
	}
}

func TestLatestInFindsNewestAcrossRoots(t *testing.T) {
	home := isolate(t)
	cwd := t.TempDir()
	primary := filepath.Join(home, "data", "hal", "sessions")
	legacy := filepath.Join(home, ".hal", "sessions")

	if _, err := LatestIn([]string{primary, legacy}, cwd); err == nil {
		t.Fatal("expected ErrNoSessions when nothing is stored")
	}

	// an older session in the legacy root
	old, err := New(legacy, cwd, "legacy", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = old.AppendMessage(agent.NewUserMessage("old"))
	_ = old.Close()

	// make the legacy session's timestamp clearly older
	oldPath := old.Path

	found, err := LatestIn([]string{primary, legacy}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if found != oldPath {
		t.Fatalf("expected the legacy session, got %s", found)
	}

	// a newer session in the canonical root wins
	time.Sleep(10 * time.Millisecond)
	newer, err := New(primary, cwd, "newer", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = newer.AppendMessage(agent.NewUserMessage("new"))
	_ = newer.Close()

	found, err = LatestIn([]string{primary, legacy}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if found != newer.Path {
		t.Fatalf("expected the newer session %s, got %s", newer.Path, found)
	}
}
