package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Rushera/hal/internal/agent"
	"github.com/Rushera/hal/internal/provider"
	"github.com/Rushera/hal/internal/tools"
)

// Several gateways (opencode.ai among them) ask coding agents to identify
// themselves and to send a stable per-conversation session id. These tests pin
// that behaviour on the wire.

func TestSessionHeaderFor(t *testing.T) {
	cases := []struct {
		configured, baseURL, want string
	}{
		{"", "https://opencode.ai/zen/go/v1", "x-opencode-session"},
		{"", "https://eu.opencode.ai/v1", "x-opencode-session"},
		{"", "https://api.openai.com/v1", ""},
		{"", "http://127.0.0.1:8080/v1", ""},
		{"", "not a url at all", ""},
		{"x-my-session", "https://api.openai.com/v1", "x-my-session"},
		{"x-my-session", "https://opencode.ai/zen/go/v1", "x-my-session"},
	}
	for _, c := range cases {
		if got := sessionHeaderFor(c.configured, c.baseURL); got != c.want {
			t.Fatalf("sessionHeaderFor(%q, %q) = %q, want %q", c.configured, c.baseURL, got, c.want)
		}
	}
}

func TestNewRunIDIsUniqueAndOpaque(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := newRunID()
		if len(id) != 16 {
			t.Fatalf("run id %q should be 16 hex chars", id)
		}
		if seen[id] {
			t.Fatalf("duplicate run id %q", id)
		}
		seen[id] = true
	}
}

// TestProviderSendsIdentityHeaders checks the HTTP layer actually transmits the
// user agent and the session header.
func TestProviderSendsIdentityHeaders(t *testing.T) {
	mock := newMockServer(t, [][]string{textChunks("ok")})

	prov := provider.NewFromEnv("k", mock.server.URL, "mock")
	prov.UserAgent = "hal/test"
	prov.SessionHeader = "x-opencode-session"
	prov.SessionID = "session-abc-123"

	toolSet, _ := tools.Select(t.TempDir(), "read")
	ag := agent.New(prov, toolSet, t.TempDir(), "")
	if err := ag.RunPrompt(context.Background(), "hi", func(agent.Event) {}); err != nil {
		t.Fatal(err)
	}

	if len(mock.headers) == 0 {
		t.Fatal("mock recorded no headers")
	}
	h := mock.headers[0]
	if got := h.Get("User-Agent"); got != "hal/test" {
		t.Fatalf("User-Agent = %q, want hal/test", got)
	}
	if got := h.Get("x-opencode-session"); got != "session-abc-123" {
		t.Fatalf("x-opencode-session = %q, want session-abc-123", got)
	}
	if got := h.Get("Authorization"); got != "Bearer k" {
		t.Fatalf("Authorization = %q", got)
	}
}

// TestProviderOmitsSessionHeaderWhenUnset keeps other endpoints unchanged.
func TestProviderOmitsSessionHeaderWhenUnset(t *testing.T) {
	mock := newMockServer(t, [][]string{textChunks("ok")})
	prov := provider.NewFromEnv("k", mock.server.URL, "mock")
	// no UserAgent, no SessionHeader configured
	ag := agent.New(prov, nil, t.TempDir(), "")
	if err := ag.RunPrompt(context.Background(), "hi", func(agent.Event) {}); err != nil {
		t.Fatal(err)
	}
	h := mock.headers[0]
	if h.Get("x-opencode-session") != "" {
		t.Fatal("session header must not be sent when it is not configured")
	}
	if h.Get("User-Agent") == "hal/test" {
		t.Fatal("test user agent leaked")
	}
}

// TestRunSendsUserAgentAndConfiguredSessionHeader drives the whole CLI: the
// user agent must always be set, and a session_header named in the config file
// must be sent with the conversation id.
func TestRunSendsUserAgentAndConfiguredSessionHeader(t *testing.T) {
	_, cfgHome, _ := isolateXDG(t)
	workdir := t.TempDir()
	mock := newMockServer(t, [][]string{textChunks("done")})

	writeConfig(t, cfgHome, "config.toml", `
model   = "m"
base_url = "`+mock.server.URL+`"
api_key = "k"
session_header = "x-opencode-session"
`)

	oldWd, _ := os.Getwd()
	if err := os.Chdir(workdir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	if _, err := captureStdout(t, func() error {
		return runWithArgs([]string{"-p", "hi"})
	}); err != nil {
		t.Fatalf("run failed: %v", err)
	}

	h := mock.headers[0]
	if got := h.Get("User-Agent"); !strings.HasPrefix(got, "hal/") {
		t.Fatalf("User-Agent = %q, want a hal/ prefix: gateways ask clients to identify themselves", got)
	}
	id := h.Get("x-opencode-session")
	if len(id) != 16 {
		t.Fatalf("x-opencode-session = %q, want a 16-char run id", id)
	}
}

// TestSessionIDStableAcrossToolCalls checks the affinity id does not change
// between the requests of a single conversation.
func TestSessionIDStableAcrossToolCalls(t *testing.T) {
	_, cfgHome, _ := isolateXDG(t)
	workdir := t.TempDir()
	mock := newMockServer(t, [][]string{
		toolChunks("c1", "write", `{"path":"x.txt","content":"y"}`, ``),
		textChunks("done"),
	})

	writeConfig(t, cfgHome, "config.toml", `
model   = "m"
base_url = "`+mock.server.URL+`"
api_key = "k"
session_header = "x-opencode-session"
tools   = "write"
`)

	oldWd, _ := os.Getwd()
	if err := os.Chdir(workdir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	if _, err := captureStdout(t, func() error {
		return runWithArgs([]string{"-p", "write the file"})
	}); err != nil {
		t.Fatalf("run failed: %v", err)
	}

	if len(mock.headers) != 2 {
		t.Fatalf("expected 2 requests (tool call + follow-up), got %d", len(mock.headers))
	}
	first := mock.headers[0].Get("x-opencode-session")
	second := mock.headers[1].Get("x-opencode-session")
	if first == "" || first != second {
		t.Fatalf("session id must be stable within a conversation: %q then %q", first, second)
	}
}

// TestResumedSessionKeepsItsAffinityID checks that -c reuses the stored session
// id rather than generating a new one, so routing affinity survives a restart.
func TestResumedSessionKeepsItsAffinityID(t *testing.T) {
	_, cfgHome, _ := isolateXDG(t)
	workdir := t.TempDir()
	mock := newMockServer(t, [][]string{
		textChunks("first"),
		textChunks("second"),
	})

	writeConfig(t, cfgHome, "config.toml", `
model   = "m"
base_url = "`+mock.server.URL+`"
api_key = "k"
session_header = "x-opencode-session"
`)

	oldWd, _ := os.Getwd()
	if err := os.Chdir(workdir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	if _, err := captureStdout(t, func() error {
		return runWithArgs([]string{"-p", "first"})
	}); err != nil {
		t.Fatal(err)
	}
	firstID := mock.headers[0].Get("x-opencode-session")

	if _, err := captureStdout(t, func() error {
		return runWithArgs([]string{"-c", "-p", "second"})
	}); err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	last := mock.headers[len(mock.headers)-1]
	secondID := last.Get("x-opencode-session")

	if firstID == "" || secondID == "" {
		t.Fatalf("session header missing: first=%q second=%q", firstID, secondID)
	}
	if firstID != secondID {
		t.Fatalf("resumed run should reuse the session id: first=%q second=%q", firstID, secondID)
	}
}
