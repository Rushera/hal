package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withEnv sets environment variables for the duration of a test. HOME is
// redirected so tests never touch the real dotfiles.
func withEnv(t *testing.T, home string, kv map[string]string) {
	t.Helper()
	t.Setenv("HOME", home)
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "HAL_CONFIG", "HAL_SESSION_DIR"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestResolveDefaultsToDotPaths(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)

	p := Resolve()
	if want := filepath.Join(home, ".config", "hal"); p.ConfigDir != want {
		t.Fatalf("ConfigDir = %s, want %s", p.ConfigDir, want)
	}
	if want := filepath.Join(home, ".local", "share", "hal", "sessions"); p.SessionDir != want {
		t.Fatalf("SessionDir = %s, want %s", p.SessionDir, want)
	}
	if want := filepath.Join(home, ".local", "state", "hal"); p.StateDir != want {
		t.Fatalf("StateDir = %s, want %s", p.StateDir, want)
	}
	if want := filepath.Join(home, ".hal"); p.LegacyDir != want {
		t.Fatalf("LegacyDir = %s, want %s", p.LegacyDir, want)
	}
}

func TestResolveHonorsXDGVariables(t *testing.T) {
	home := t.TempDir()
	cfgHome := filepath.Join(home, "xdg-config")
	dataHome := filepath.Join(home, "xdg-data")
	stateHome := filepath.Join(home, "xdg-state")
	withEnv(t, home, map[string]string{
		"XDG_CONFIG_HOME": cfgHome,
		"XDG_DATA_HOME":   dataHome,
		"XDG_STATE_HOME":  stateHome,
	})

	p := Resolve()
	if p.ConfigDir != filepath.Join(cfgHome, "hal") {
		t.Fatalf("ConfigDir = %s", p.ConfigDir)
	}
	if p.DataDir != filepath.Join(dataHome, "hal") {
		t.Fatalf("DataDir = %s", p.DataDir)
	}
	if p.SessionDir != filepath.Join(dataHome, "hal", "sessions") {
		t.Fatalf("SessionDir = %s", p.SessionDir)
	}
	if p.StateDir != filepath.Join(stateHome, "hal") {
		t.Fatalf("StateDir = %s", p.StateDir)
	}
}

// The XDG spec says relative paths in the XDG_* variables are invalid and must
// be ignored.
func TestResolveIgnoresRelativeXDGPaths(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, map[string]string{
		"XDG_CONFIG_HOME": "relative/config",
		"XDG_DATA_HOME":   "./data",
	})
	p := Resolve()
	if p.ConfigDir != filepath.Join(home, ".config", "hal") {
		t.Fatalf("relative XDG_CONFIG_HOME should be ignored, got %s", p.ConfigDir)
	}
	if p.DataDir != filepath.Join(home, ".local", "share", "hal") {
		t.Fatalf("relative XDG_DATA_HOME should be ignored, got %s", p.DataDir)
	}
}

func TestLoadTOML(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)
	p := Resolve()
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `
# hal configuration
model = "gpt-4o"
base_url = "https://example.test/v1"
tools = "read,grep"
max_turns = 7
no_context_files = true
session_dir = "/tmp/sessions"
`
	if err := os.WriteFile(filepath.Join(p.ConfigDir, "config.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load("", p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Path == "" {
		t.Fatal("config file not found")
	}
	f := cfg.File
	if f.Model != "gpt-4o" || f.BaseURL != "https://example.test/v1" || f.Tools != "read,grep" {
		t.Fatalf("strings not parsed: %+v", f)
	}
	if f.MaxTurns != 7 {
		t.Fatalf("max_turns = %d", f.MaxTurns)
	}
	if f.NoContextFiles == nil || !*f.NoContextFiles {
		t.Fatal("no_context_files = true not parsed")
	}
	if f.SessionDir != "/tmp/sessions" {
		t.Fatalf("session_dir = %q", f.SessionDir)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", cfg.Warnings)
	}
}

func TestLoadJSON(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)
	p := Resolve()
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"model":"claude-x","max_turns":3,"no_session":false}`
	if err := os.WriteFile(filepath.Join(p.ConfigDir, "config.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("", p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.File.Model != "claude-x" || cfg.File.MaxTurns != 3 {
		t.Fatalf("json not parsed: %+v", cfg.File)
	}
	if cfg.File.NoSession == nil || *cfg.File.NoSession {
		t.Fatal("no_session should be parsed as false")
	}
}

// TOML normally wins over JSON in the same directory.
func TestTOMLBeatsJSON(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)
	p := Resolve()
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(p.ConfigDir, "config.toml"), []byte(`model = "from-toml"`), 0o600)
	os.WriteFile(filepath.Join(p.ConfigDir, "config.json"), []byte(`{"model":"from-json"}`), 0o600)

	cfg, err := Load("", p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.File.Model != "from-toml" {
		t.Fatalf("expected config.toml to win, got %q", cfg.File.Model)
	}
}

func TestLegacyConfigIsUsedWhenXDGAbsent(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)
	p := Resolve()
	if err := os.MkdirAll(p.LegacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.LegacyDir, "config.toml"), []byte(`model = "legacy-model"`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("", p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.File.Model != "legacy-model" {
		t.Fatalf("legacy config not read: %+v", cfg.File)
	}
}

func TestExplicitPathWinsAndMissingPathIsAnError(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)
	p := Resolve()
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(p.ConfigDir, "config.toml"), []byte(`model = "from-xdg"`), 0o600)

	other := filepath.Join(home, "custom.toml")
	os.WriteFile(other, []byte(`model = "from-custom"`), 0o600)

	cfg, err := Load(other, p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.File.Model != "from-custom" || cfg.Path != other {
		t.Fatalf("explicit path not honored: %+v", cfg)
	}

	if _, err := Load(filepath.Join(home, "does-not-exist.toml"), p); err == nil {
		t.Fatal("an explicit missing config file must be an error")
	}
}

func TestMissingConfigIsNotAnError(t *testing.T) {
	withEnv(t, t.TempDir(), nil)
	cfg, err := Load("", Resolve())
	if err != nil {
		t.Fatalf("a missing config file must not error: %v", err)
	}
	if cfg.Path != "" || len(cfg.Warnings) != 0 {
		t.Fatalf("expected an empty config, got %+v", cfg)
	}
}

func TestBrokenConfigReportsPath(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)
	p := Resolve()
	os.MkdirAll(p.ConfigDir, 0o755)
	bad := filepath.Join(p.ConfigDir, "config.toml")
	os.WriteFile(bad, []byte("model = "), 0o600)

	_, err := Load("", p)
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if !strings.Contains(err.Error(), bad) {
		t.Fatalf("error should name the file: %v", err)
	}
}

func TestUnknownKeysAreWarnedAbout(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)
	p := Resolve()
	os.MkdirAll(p.ConfigDir, 0o755)
	os.WriteFile(filepath.Join(p.ConfigDir, "config.toml"), []byte("model = \"m\"\nmodle = \"typo\"\n"), 0o600)

	cfg, err := Load("", p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) == 0 || !strings.Contains(cfg.Warnings[0], "modle") {
		t.Fatalf("expected a warning about the unknown key, got %v", cfg.Warnings)
	}
}

func TestWorldReadableConfigWithKeyWarns(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)
	p := Resolve()
	os.MkdirAll(p.ConfigDir, 0o755)
	path := filepath.Join(p.ConfigDir, "config.toml")
	os.WriteFile(path, []byte("api_key = \"sk-abcdefghijklmnop\"\n"), 0o600)
	// chmod explicitly: the ambient umask (0077 here) would otherwise strip the
	// group/other bits and the warning could never trigger.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load("", p)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "readable by other users") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a permissions warning, got %v", cfg.Warnings)
	}

	// tightening the permissions silences it
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load("", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "readable by other users") {
			t.Fatal("600 permissions should not warn")
		}
	}
}

func TestSessionDirsFollowXDG(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)
	p := Resolve()

	// the canonical session location is always under XDG_DATA_HOME
	if want := filepath.Join(home, ".local", "share", "hal", "sessions"); p.SessionDir != want {
		t.Fatalf("SessionDir = %s, want %s", p.SessionDir, want)
	}
	// the legacy location is still exposed so sessions can be resumed from it
	if want := filepath.Join(home, ".hal", "sessions"); p.LegacySessionDir != want {
		t.Fatalf("LegacySessionDir = %s, want %s", p.LegacySessionDir, want)
	}
	// creating legacy data must not move the canonical location
	if err := os.MkdirAll(p.LegacySessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Resolve().SessionDir; got != p.SessionDir {
		t.Fatalf("SessionDir moved to legacy data: %s", got)
	}
}

func TestConfigPathEnvOverride(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, map[string]string{"HAL_CONFIG": "/etc/hal/custom.toml"})
	if got := Resolve().ConfigPath(); got != "/etc/hal/custom.toml" {
		t.Fatalf("ConfigPath = %s", got)
	}
}

func TestExpandPath(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)

	cases := map[string]string{
		"":              "",
		"/abs/path":     "/abs/path",
		"relative/path": "relative/path",
		"~":             home,
		"~/sessions":    filepath.Join(home, "sessions"),
		"~/a/b/c":       filepath.Join(home, "a", "b", "c"),
		"~otheruser/x":  "~otheruser/x", // not ours to resolve
	}
	for in, want := range cases {
		if got := ExpandPath(in); got != want {
			t.Fatalf("ExpandPath(%q) = %q, want %q", in, got, want)
		}
	}
}
