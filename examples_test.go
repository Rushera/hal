package main

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Rushera/hal/internal/config"
	"github.com/Rushera/hal/internal/session"
)

// The example config files are documentation that users copy. These tests keep
// them honest: every key must be known to the schema, both formats must be
// accepted, and the two must describe the same configuration.

func loadExample(t *testing.T, name string) config.Loaded {
	t.Helper()
	path := filepath.Join("examples", name)
	loaded, err := config.Load(path, config.Resolve())
	if err != nil {
		t.Fatalf("%s does not parse: %v", path, err)
	}
	if loaded.Path != path {
		t.Fatalf("%s was not loaded (got %q)", path, loaded.Path)
	}
	// Unknown keys are surfaced as warnings, so a warning means the example has
	// drifted from the schema.
	for _, w := range loaded.Warnings {
		t.Fatalf("%s has a schema problem: %s", path, w)
	}
	return loaded
}

// TestExampleConfigsAreSchemaAccurate checks that every documented key is real
// and that the documented values survive the round trip.
func TestExampleConfigsAreSchemaAccurate(t *testing.T) {
	tomlCfg := loadExample(t, "config.toml")

	want := config.File{
		Provider:           "openai",
		Model:              "gpt-4o-mini",
		BaseURL:            "https://api.openai.com/v1",
		Tools:              "read,bash,edit,write,grep,find,ls",
		MaxTurns:           32,
		AppendSystemPrompt: "Prefer small, focused changes and run the tests before finishing.",
	}
	got := tomlCfg.File
	if got.NoSession == nil || got.NoContextFiles == nil {
		t.Fatal("examples/config.toml must set both booleans explicitly")
	}
	got.NoSession, got.NoContextFiles = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("examples/config.toml drifted:\n got %+v\nwant %+v", got, want)
	}
}

// TestExampleJSONMatchesTOML guards against the two formats documenting different
// settings.
func TestExampleJSONMatchesTOML(t *testing.T) {
	tomlCfg := loadExample(t, "config.toml").File
	jsonCfg := loadExample(t, "config.json").File
	if !reflect.DeepEqual(tomlCfg, jsonCfg) {
		t.Fatalf("examples/config.json and examples/config.toml disagree:\n toml %+v\n json %+v", tomlCfg, jsonCfg)
	}
}

// TestExampleOpencodeGoPreset checks the Hermes-matching preset resolves to the
// gateway endpoint, model and credential variable documented in it.
func TestExampleOpencodeGoPreset(t *testing.T) {
	isolateXDG(t)
	cfg := loadExample(t, "config.opencode-go.toml")

	if cfg.File.BaseURL != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("base_url = %q", cfg.File.BaseURL)
	}
	if cfg.File.Model != "deepseek-v4.1-flash" {
		t.Fatalf("model = %q", cfg.File.Model)
	}
	if cfg.File.APIKeyEnv != "OPENCODE_GO_API_KEY" {
		t.Fatalf("api_key_env = %q", cfg.File.APIKeyEnv)
	}
	if cfg.File.APIKey != "" {
		t.Fatal("the preset must not embed a literal key")
	}
	if cfg.File.MaxTurns != 90 {
		t.Fatalf("max_turns = %d", cfg.File.MaxTurns)
	}

	// the named variable supplies the credential
	t.Setenv("OPENCODE_GO_API_KEY", "from-named-env")
	opts, set, err := parseArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, sources := resolveOptions(opts, set, cfg)
	if resolved.apiKey != "from-named-env" {
		t.Fatalf("api key not read from api_key_env: %q", resolved.apiKey)
	}
	if resolved.model != "deepseek-v4.1-flash" || resolved.baseURL != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("preset not applied: model=%q base_url=%q", resolved.model, resolved.baseURL)
	}
	for _, s := range sources {
		if s.Key == "api_key" && s.Source != "env $OPENCODE_GO_API_KEY" {
			t.Fatalf("api_key source = %q, want env $OPENCODE_GO_API_KEY", s.Source)
		}
	}
}

// TestAPIKeyEnvPrecedence pins the order: flag > named variable > generic
// variables > literal key in the file.
func TestAPIKeyEnvPrecedence(t *testing.T) {
	isolateXDG(t)
	cfg := config.Loaded{
		Path: "/tmp/config.toml",
		File: config.File{APIKeyEnv: "NAMED_KEY", APIKey: "literal-in-file"},
	}

	// literal in the file is the last resort
	opts, set, err := parseArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := resolveOptions(opts, set, cfg)
	if resolved.apiKey != "literal-in-file" {
		t.Fatalf("literal key should be the fallback: %q", resolved.apiKey)
	}

	// the named variable wins over it
	t.Setenv("NAMED_KEY", "named")
	resolved, _ = resolveOptions(opts, set, cfg)
	if resolved.apiKey != "named" {
		t.Fatalf("named variable should win: %q", resolved.apiKey)
	}

	// so do the generic ones when no name is configured
	cfg2 := config.Loaded{Path: "/tmp/config.toml"}
	t.Setenv("OPENCODE_GO_API_KEY", "opencode")
	t.Setenv("HAL_API_KEY", "generic")
	resolved, _ = resolveOptions(opts, set, cfg2)
	if resolved.apiKey != "generic" {
		t.Fatalf("HAL_API_KEY should win: %q", resolved.apiKey)
	}

	// a flag beats everything
	opts, set, err = parseArgs([]string{"--api-key", "from-flag"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ = resolveOptions(opts, set, cfg)
	if resolved.apiKey != "from-flag" {
		t.Fatalf("flag should win: %q", resolved.apiKey)
	}
}

// TestExampleMockConfigPreset checks the mock preset is usable as documented.
func TestExampleMockConfigPreset(t *testing.T) {
	cfg := loadExample(t, "config.mock.toml").File

	if cfg.BaseURL != "http://127.0.0.1:8792/v1" {
		t.Fatalf("mock base_url = %q", cfg.BaseURL)
	}
	if cfg.Model != "mock-model" || cfg.APIKey == "" {
		t.Fatalf("mock preset incomplete: %+v", cfg)
	}
	if cfg.Tools != "read,write,edit,bash,grep,find,ls" {
		t.Fatalf("mock tools = %q", cfg.Tools)
	}
	if cfg.MaxTurns != 8 {
		t.Fatalf("mock max_turns = %d", cfg.MaxTurns)
	}
	// the documented session_dir uses a leading ~, which must expand
	if cfg.SessionDir == "" || cfg.SessionDir[0] != '~' {
		t.Fatalf("mock session_dir should demonstrate tilde expansion: %q", cfg.SessionDir)
	}
	expanded := config.ExpandPath(cfg.SessionDir)
	if expanded == cfg.SessionDir || !filepath.IsAbs(expanded) {
		t.Fatalf("session_dir %q did not expand to an absolute path (got %q)", cfg.SessionDir, expanded)
	}
}

// TestExamplesDriveResolution confirms the mock preset actually flows through
// precedence, so `--config examples/config.mock.toml` yields those values.
func TestExamplesDriveResolution(t *testing.T) {
	isolateXDG(t)
	cfg := loadExample(t, "config.mock.toml")

	opts, set, err := parseArgs([]string{"--config", filepath.Join("examples", "config.mock.toml")})
	if err != nil {
		t.Fatal(err)
	}
	resolved, sources := resolveOptions(opts, set, cfg)

	if resolved.baseURL != "http://127.0.0.1:8792/v1" {
		t.Fatalf("mock base_url not resolved: %q", resolved.baseURL)
	}
	if resolved.maxTurns != 8 {
		t.Fatalf("mock max_turns not resolved: %d", resolved.maxTurns)
	}
	// the value stays as written for --show-config reporting, and expands when used
	if resolved.sessionDir != "~/.local/share/hal/mock-sessions" {
		t.Fatalf("session_dir should be reported as written: %q", resolved.sessionDir)
	}
	if got, want := session.RootWith(resolved.sessionDir), config.ExpandPath("~/.local/share/hal/mock-sessions"); got != want {
		t.Fatalf("mock session_dir did not expand: got %s want %s", got, want)
	}

	src := map[string]string{}
	for _, s := range sources {
		src[s.Key] = s.Source
	}
	if src["base_url"] == "default" || src["model"] == "default" {
		t.Fatalf("mock preset should supply base_url and model: %v", src)
	}
}
