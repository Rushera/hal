package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Rushera/hal/internal/agent"
	"github.com/Rushera/hal/internal/provider"
	"github.com/Rushera/hal/internal/session"
	"github.com/Rushera/hal/internal/tools"
)

// newSessionFor creates a session in the configured store for the given cwd.
func newSessionFor(t *testing.T, cwd, name string) (*session.Session, error) {
	t.Helper()
	return session.New(session.RootWith(""), cwd, name, "mock")
}

// mockServer scripted like a provider: every response is a list of SSE chunks.
type mockServer struct {
	t        *testing.T
	script   [][]string
	requests []map[string]any
	headers  []http.Header
	server   *httptest.Server
	call     int
}

func newMockServer(t *testing.T, script [][]string) *mockServer {
	m := &mockServer{t: t, script: script}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		m.t.Errorf("mock received invalid JSON: %v", err)
	}
	m.requests = append(m.requests, req)
	m.headers = append(m.headers, r.Header.Clone())

	if m.call >= len(m.script) {
		m.t.Errorf("mock got more requests (%d) than scripted", m.call+1)
		http.Error(w, "no scripted response", http.StatusInternalServerError)
		return
	}
	chunks := m.script[m.call]
	m.call++

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
		if flusher != nil {
			flusher.Flush()
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func textChunks(texts ...string) []string {
	var out []string
	for _, s := range texts {
		out = append(out, fmt.Sprintf(`{"choices":[{"delta":{"content":%s},"finish_reason":null}]}`, mustJSON(s)))
	}
	out = append(out, `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`)
	return out
}

// toolChunks streams a tool call split across chunks (like real providers do).
func toolChunks(id, name, argsFragment1, argsFragment2 string) []string {
	return []string{
		fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":%s,"function":{"name":%s,"arguments":%s}}]},"finish_reason":null}]}`,
			mustJSON(id), mustJSON(name), mustJSON(argsFragment1)),
		fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":%s}}]},"finish_reason":null}]}`, mustJSON(argsFragment2)),
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":9,"total_tokens":29}}`,
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestAgentLoopExecutesToolAndFinishes is the core end-to-end test: a scripted
// provider asks for the write tool, the real tool runs against a real temp
// directory, and the follow-up request must contain the tool result.
func TestAgentLoopExecutesToolAndFinishes(t *testing.T) {
	workdir := t.TempDir()
	mock := newMockServer(t, [][]string{
		toolChunks("call_1", "write", `{"path":"hello.txt","content":"hi from the tool\n"}`, ``),
		textChunks("Done: ", "wrote hello.txt"),
	})

	prov := provider.NewFromEnv("test-key", mock.server.URL, "mock-model")
	if prov.Name() != "openai-compatible" {
		t.Fatalf("unexpected provider name %q", prov.Name())
	}
	toolSet, err := tools.Select(workdir, "")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(prov, toolSet, workdir, "")

	var events []agent.Event
	err = ag.RunPrompt(context.Background(), "create hello.txt", func(ev agent.Event) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatalf("agent loop failed: %v", err)
	}

	// the tool really ran against the filesystem
	data, err := os.ReadFile(filepath.Join(workdir, "hello.txt"))
	if err != nil {
		t.Fatalf("tool did not write the file: %v", err)
	}
	if string(data) != "hi from the tool\n" {
		t.Fatalf("wrong file content: %q", data)
	}

	// the provider saw two requests: the tool result must be in the second
	if mock.call != 2 {
		t.Fatalf("expected 2 provider requests, got %d", mock.call)
	}
	second := mock.requests[1]
	msgs, _ := second["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("second request had no messages")
	}
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "tool" {
		t.Fatalf("last message should be the tool result, got %v", last["role"])
	}
	if last["tool_call_id"] != "call_1" {
		t.Fatalf("tool_call_id not propagated: %v", last["tool_call_id"])
	}
	if !strings.Contains(fmt.Sprint(last["content"]), "wrote 17 bytes to hello.txt") {
		t.Fatalf("tool result content missing: %v", last["content"])
	}

	// tools were advertised on the first request
	first := mock.requests[0]
	declared, _ := first["tools"].([]any)
	if len(declared) != 7 {
		t.Fatalf("expected 7 tool definitions, got %d", len(declared))
	}
	if first["stream"] != true {
		t.Fatal("streaming was not requested")
	}

	// event stream shape: deltas, tool start/result, turn end, done, usage
	var sawDelta, sawToolStart, sawToolResult, sawTurnEnd, sawDone, sawUsage bool
	var streamed strings.Builder
	for _, ev := range events {
		switch ev.Type {
		case agent.EventTextDelta:
			sawDelta = true
			streamed.WriteString(ev.Text)
		case agent.EventToolStart:
			sawToolStart = true
			if ev.ToolCall == nil || ev.ToolCall.Name != "write" {
				t.Fatalf("bad tool start event: %+v", ev.ToolCall)
			}
		case agent.EventToolResult:
			sawToolResult = true
			if ev.Result == nil || ev.Result.IsError {
				t.Fatalf("tool result should be successful: %+v", ev.Result)
			}
		case agent.EventTurnEnd:
			sawTurnEnd = true
		case agent.EventUsage:
			sawUsage = true
		case agent.EventDone:
			sawDone = true
		}
	}
	if !sawDelta || !sawToolStart || !sawToolResult || !sawTurnEnd || !sawUsage || !sawDone {
		t.Fatalf("missing events: delta=%v toolStart=%v toolResult=%v turnEnd=%v usage=%v done=%v",
			sawDelta, sawToolStart, sawToolResult, sawTurnEnd, sawUsage, sawDone)
	}
	if streamed.String() != "Done: wrote hello.txt" {
		t.Fatalf("streamed text mismatch: %q", streamed.String())
	}

	// History is the conversation only: the system prompt is added when the
	// request is assembled, not stored in history.
	hist := ag.Messages()
	if len(hist) != 4 {
		t.Fatalf("expected 4 history messages, got %d: %+v", len(hist), roles(hist))
	}
	if hist[1].Role != agent.RoleAssistant || len(hist[1].ToolCalls) != 1 || hist[1].ToolCalls[0].Name != "write" {
		t.Fatalf("assistant tool call not recorded: %+v", hist[1])
	}
	if hist[2].Role != agent.RoleTool || hist[2].Content == "" {
		t.Fatalf("tool result not recorded: %+v", hist[2])
	}
	if hist[3].Role != agent.RoleAssistant || hist[3].Content != "Done: wrote hello.txt" {
		t.Fatalf("final assistant message not recorded: %+v", hist[3])
	}
	// the system prompt is still prepended to requests
	if msgs, _ := mock.requests[0]["messages"].([]any); len(msgs) == 0 {
		t.Fatal("no messages sent")
	} else if first, _ := msgs[0].(map[string]any); first["role"] != "system" {
		t.Fatalf("first request message should be the system prompt, got %v", first["role"])
	}
	if !strings.Contains(ag.SystemPrompt, workdir) {
		t.Fatalf("system prompt should mention the working directory")
	}
}

func roles(msgs []agent.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Role)
	}
	return out
}

// TestAgentLoopChainsMultipleTools covers read -> edit in one run.
func TestAgentLoopChainsMultipleTools(t *testing.T) {
	workdir := t.TempDir()
	target := filepath.Join(workdir, "config.go")
	if err := os.WriteFile(target, []byte("timeout := 30\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := newMockServer(t, [][]string{
		toolChunks("c1", "read", `{"path":"config.go"}`, ``),
		toolChunks("c2", "edit", `{"path":"config.go",`, `"oldText":"timeout := 30","newText":"timeout := 60"}`),
		textChunks("Bumped the timeout to 60s."),
	})

	prov := provider.NewFromEnv("k", mock.server.URL, "mock")
	toolSet, _ := tools.Select(workdir, "")
	ag := agent.New(prov, toolSet, workdir, "")

	if err := ag.RunPrompt(context.Background(), "bump the timeout", func(agent.Event) {}); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "timeout := 60\n" {
		t.Fatalf("edit not applied: %q", data)
	}
	if mock.call != 3 {
		t.Fatalf("expected 3 provider requests, got %d", mock.call)
	}
}

// TestToolFailureIsFedBackToTheModel checks errors become tool results, not
// aborts.
func TestToolFailureIsFedBackToTheModel(t *testing.T) {
	workdir := t.TempDir()
	mock := newMockServer(t, [][]string{
		toolChunks("c1", "read", `{"path":"missing.txt"}`, ``),
		textChunks("That file does not exist."),
	})
	prov := provider.NewFromEnv("k", mock.server.URL, "mock")
	toolSet, _ := tools.Select(workdir, "")
	ag := agent.New(prov, toolSet, workdir, "")

	var sawError bool
	err := ag.RunPrompt(context.Background(), "read missing.txt", func(ev agent.Event) {
		if ev.Type == agent.EventToolResult && ev.Result != nil && ev.Result.IsError {
			sawError = true
		}
	})
	if err != nil {
		t.Fatalf("a failing tool must not abort the run: %v", err)
	}
	if !sawError {
		t.Fatal("expected an error tool result")
	}
	last := ag.Messages()[len(ag.Messages())-1]
	if !strings.Contains(last.Content, "does not exist") && last.Role != agent.RoleAssistant {
		t.Fatalf("unexpected last message: %+v", last)
	}
}

// TestToolAllowlistLimitsAdvertisedTools covers `--tools read,grep`.
func TestToolAllowlistLimitsAdvertisedTools(t *testing.T) {
	workdir := t.TempDir()
	mock := newMockServer(t, [][]string{textChunks("nothing to do")})
	prov := provider.NewFromEnv("k", mock.server.URL, "mock")
	toolSet, err := tools.Select(workdir, "read,grep")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(prov, toolSet, workdir, "")
	if err := ag.RunPrompt(context.Background(), "hi", func(agent.Event) {}); err != nil {
		t.Fatal(err)
	}
	declared, _ := mock.requests[0]["tools"].([]any)
	if len(declared) != 2 {
		t.Fatalf("expected 2 tools advertised, got %d", len(declared))
	}
}

// TestProviderHTTPErrorIsReported checks a non-2xx response surfaces cleanly.
func TestProviderHTTPErrorIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	prov := provider.NewFromEnv("k", srv.URL, "mock")
	ag := agent.New(prov, nil, t.TempDir(), "")
	err := ag.RunPrompt(context.Background(), "hi", func(agent.Event) {})
	if err == nil {
		t.Fatal("expected an error for HTTP 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error should mention the status: %v", err)
	}
}

// TestParseArgs covers CLI flag handling.
func TestParseArgs(t *testing.T) {
	o, set, err := parseArgs([]string{"-p", "--model", "gpt-4o", "--tools", "read,grep", "@notes.md", "summarize", "this"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.printMode || o.model != "gpt-4o" || o.toolList != "read,grep" {
		t.Fatalf("flags not parsed: %+v", o)
	}
	if !set["p"] || !set["model"] || !set["tools"] {
		t.Fatalf("explicitly set flags not tracked: %v", set)
	}
	if set["no-session"] {
		t.Fatalf("flags that were not typed must not be recorded as set: %v", set)
	}
	if len(o.files) != 1 || o.files[0] != "notes.md" {
		t.Fatalf("file arg not parsed: %+v", o.files)
	}
	if strings.Join(o.prompts, " ") != "summarize this" {
		t.Fatalf("prompt not assembled: %+v", o.prompts)
	}

	if _, _, err := parseArgs([]string{"--mode", "yaml"}); err == nil {
		t.Fatal("expected an error for an unknown mode")
	}

	// an unsupported provider is rejected after resolution, not during parsing
	o, _, err = parseArgs([]string{"--provider", "anthropic"})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateResolved(o); err == nil {
		t.Fatal("expected validateResolved to reject a non-bundled provider")
	}
}

func TestBuildPromptMergesFilesAndStdin(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("- [ ] task one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := buildPrompt([]string{"extract the todos"}, []string{"notes.md"}, dir, "PIPED CONTENT")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PIPED CONTENT", `<file path="notes.md">`, "task one", "extract the todos"} {
		if !strings.Contains(out, want) {
			t.Fatalf("prompt missing %q:\n%s", want, out)
		}
	}
	if _, err := buildPrompt(nil, []string{"nope.md"}, dir, ""); err == nil {
		t.Fatal("expected an error for a missing @file")
	}
}

// TestRunPrintStreamsToStdout exercises the print-mode path end to end.
func TestRunPrintStreamsToStdout(t *testing.T) {
	workdir := t.TempDir()
	mock := newMockServer(t, [][]string{
		toolChunks("c1", "bash", `{"command":"echo tool-ran > out.txt"}`, ``),
		textChunks("Ran ", "the command."),
	})
	prov := provider.NewFromEnv("k", mock.server.URL, "mock")
	toolSet, _ := tools.Select(workdir, "")
	ag := agent.New(prov, toolSet, workdir, "")

	var stdout, stderr bytes.Buffer
	if err := runPrint(context.Background(), ag, "run echo", &stdout, &stderr); err != nil {
		t.Fatalf("print mode failed: %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "Ran the command.") {
		t.Fatalf("stdout should stream the assistant text, got %q", got)
	}
	if !strings.Contains(stderr.String(), "bash(") {
		t.Fatalf("stderr should report tool activity, got %q", stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(workdir, "out.txt"))
	if err != nil || !strings.Contains(string(data), "tool-ran") {
		t.Fatalf("bash tool did not execute: %v %q", err, data)
	}
}

// TestRunJSONEmitsEventStream covers `--mode json`.
func TestRunJSONEmitsEventStream(t *testing.T) {
	mock := newMockServer(t, [][]string{textChunks("json mode works")})
	prov := provider.NewFromEnv("k", mock.server.URL, "mock")
	ag := agent.New(prov, nil, t.TempDir(), "")

	out := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := runJSON(context.Background(), ag, "hi")
	w.Close()
	os.Stdout = out
	if runErr != nil {
		t.Fatal(runErr)
	}
	data, _ := io.ReadAll(r)

	var types []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var ev agent.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("invalid JSON line %q: %v", line, err)
		}
		types = append(types, ev.Type)
	}
	joined := strings.Join(types, ",")
	if !strings.Contains(joined, agent.EventTextDelta) || !strings.Contains(joined, agent.EventDone) {
		t.Fatalf("event stream incomplete: %s", joined)
	}
}

func TestContextFilesIncludeGlobalConfigDir(t *testing.T) {
	_, cfgHome, _ := isolateXDG(t)
	workdir := t.TempDir()

	globalDir := filepath.Join(cfgHome, "hal")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "AGENTS.md"), []byte("global rules"), 0o600); err != nil {
		t.Fatal(err)
	}

	files, err := contextFiles(workdir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range files {
		if strings.HasSuffix(f.path, filepath.Join("hal", "AGENTS.md")) {
			found = true
			if !strings.Contains(f.label, "XDG_CONFIG_HOME") {
				t.Fatalf("global context file not labelled as the config dir: %q", f.label)
			}
		}
	}
	if !found {
		t.Fatalf("AGENTS.md in $XDG_CONFIG_HOME/hal was not loaded: %+v", files)
	}

	// the content reaches the system prompt
	opts := options{}
	prompt, err := buildSystemPrompt(workdir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "global rules") {
		t.Fatalf("global instructions missing from the system prompt:\n%s", prompt)
	}

	// --no-context-files suppresses them
	opts.noContext = true
	prompt, err = buildSystemPrompt(workdir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "global rules") {
		t.Fatal("--no-context-files should suppress context files")
	}
}

func TestContextFilesDiscovery(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "project")
	if err := os.MkdirAll(filepath.Join(child, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "AGENTS.md"), []byte("parent rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "AGENTS.md"), []byte("child rules"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := contextFiles(filepath.Join(child, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	var foundParent, foundChild bool
	for _, f := range files {
		if strings.HasSuffix(f.path, filepath.Join(parent, "AGENTS.md")) {
			foundParent = true
		}
		if strings.HasSuffix(f.path, filepath.Join(child, "AGENTS.md")) {
			foundChild = true
		}
	}
	if !foundParent || !foundChild {
		t.Fatalf("expected both context files, got %+v", files)
	}
	if len(files) > 1 && !strings.Contains(files[len(files)-1].path, "sub") {
		// the innermost directory must be searched last so it wins
		for i := 0; i < len(files)-1; i++ {
			if strings.Contains(files[i].path, "child") && strings.Contains(files[i+1].path, "parent") {
				t.Fatalf("context files not ordered outer-to-inner: %+v", files)
			}
		}
	}
}

func TestSessionPersistenceThroughAgent(t *testing.T) {
	root := t.TempDir()
	workdir := t.TempDir()
	t.Setenv("HAL_SESSION_DIR", root)

	mock := newMockServer(t, [][]string{textChunks("noted")})
	prov := provider.NewFromEnv("k", mock.server.URL, "mock")
	toolSet, _ := tools.Select(workdir, "")
	ag := agent.New(prov, toolSet, workdir, "")

	sess, err := newSessionFor(t, workdir, "cli session")
	if err != nil {
		t.Fatal(err)
	}
	ag.OnMessage = func(m agent.Message) { _ = sess.AppendMessage(m) }
	if err := ag.RunPrompt(context.Background(), "remember this", func(agent.Event) {}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 { // session header, user, assistant
		t.Fatalf("expected 3 session lines, got %d:\n%s", len(lines), data)
	}
	if !strings.Contains(lines[0], `"type":"session"`) || !strings.Contains(lines[1], `"role":"user"`) {
		t.Fatalf("unexpected session file:\n%s", data)
	}
}
