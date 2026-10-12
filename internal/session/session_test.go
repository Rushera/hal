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
	if explicit := filepath.Join(t.TempDir(), "explicit"); RootWith(explicit) != explicit {
		t.Fatalf("an explicit directory must win: %s", RootWith(explicit))
	}
	fromEnv := filepath.Join(t.TempDir(), "from-env")
	t.Setenv("HAL_SESSION_DIR", fromEnv)
	if got := RootWith(""); got != fromEnv {
		t.Fatalf("HAL_SESSION_DIR must win over XDG: %s", got)
	}
}

func TestSearchRootsIsTheResolvedRoot(t *testing.T) {
	home := isolate(t)
	roots := SearchRoots("")
	if len(roots) != 1 {
		t.Fatalf("expected exactly one root, got %v", roots)
	}
	if want := filepath.Join(home, "data", "hal", "sessions"); roots[0] != want {
		t.Fatalf("root = %s, want %s", roots[0], want)
	}
}

func TestLatestInFindsNewestAcrossRoots(t *testing.T) {
	home := isolate(t)
	cwd := t.TempDir()
	primary := filepath.Join(home, "data", "hal", "sessions")
	secondary := filepath.Join(home, "other-root")

	if _, err := LatestIn([]string{primary, secondary}, cwd); err == nil {
		t.Fatal("expected ErrNoSessions when nothing is stored")
	}

	// an older session in the secondary root
	old, err := New(secondary, cwd, "older", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = old.AppendMessage(agent.NewUserMessage("old"))
	_ = old.Close()

	oldPath := old.Path

	found, err := LatestIn([]string{primary, secondary}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if found != oldPath {
		t.Fatalf("expected the session in the secondary root, got %s", found)
	}

	// a newer session in the primary root wins
	time.Sleep(10 * time.Millisecond)
	newer, err := New(primary, cwd, "newer", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = newer.AppendMessage(agent.NewUserMessage("new"))
	_ = newer.Close()

	found, err = LatestIn([]string{primary, secondary}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if found != newer.Path {
		t.Fatalf("expected the newer session %s, got %s", newer.Path, found)
	}
}

func TestFindByIDAcceptsFullAndShortIDs(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	cwd := t.TempDir()

	s, err := New(root, cwd, "named", "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(agent.NewUserMessage("hello")); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	for _, id := range []string{s.ID, s.ID[:8]} {
		path, err := FindByID(root, id)
		if err != nil {
			t.Fatalf("FindByID(%q): %v", id, err)
		}
		if path != s.Path {
			t.Fatalf("FindByID(%q) = %s, want %s", id, path, s.Path)
		}
	}

	// a different project directory is searched too, not just the current cwd
	other, err := New(root, t.TempDir(), "other", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = other.Close()
	if path, err := FindByID(root, other.ID); err != nil || path != other.Path {
		t.Fatalf("FindByID across project dirs = %q, %v", path, err)
	}

	if _, err := FindByID(root, "0123456789abcdef"); err == nil {
		t.Fatal("expected an error for an unknown id")
	}
	if _, err := FindByID(root, ""); err == nil {
		t.Fatal("expected an error for an empty id")
	}
	if _, err := FindByID(filepath.Join(root, "absent"), s.ID); err == nil {
		t.Fatal("expected an error when the root does not exist")
	}
}
