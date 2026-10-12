package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Rushera/hal/internal/agent"
)

// fakeAgent is a scripted stand-in for *agent.Agent.
type fakeAgent struct {
	mu       sync.Mutex
	script   []agent.Event
	blocking bool
	prompts  []string
	model    string
	aborted  bool
	failWith error
}

func (f *fakeAgent) RunPrompt(ctx context.Context, prompt string, emit func(agent.Event)) error {
	f.mu.Lock()
	f.prompts = append(f.prompts, prompt)
	script := append([]agent.Event(nil), f.script...)
	failWith := f.failWith
	blocking := f.blocking
	f.mu.Unlock()

	for _, ev := range script {
		emit(ev)
	}
	if blocking {
		<-ctx.Done()
		f.mu.Lock()
		f.aborted = true
		f.mu.Unlock()
		return ctx.Err()
	}
	if failWith != nil {
		emit(agent.Event{Type: agent.EventError, Error: failWith.Error()})
		return failWith
	}
	return nil
}

func (f *fakeAgent) ModelName() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.model == "" {
		return "fake-model"
	}
	return f.model
}
func (f *fakeAgent) SetModelName(m string) bool { f.mu.Lock(); f.model = m; f.mu.Unlock(); return true }
func (f *fakeAgent) ProviderName() string       { return "fake" }
func (f *fakeAgent) Messages() []agent.Message  { return nil }
func (f *fakeAgent) Reset()                     {}
func (f *fakeAgent) ToolNames() []string        { return []string{"read", "bash", "edit", "write"} }

func (f *fakeAgent) promptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

func (f *fakeAgent) wasAborted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.aborted
}

// newTestModel builds a sized model plus its event channel.
func newTestModel(t *testing.T, a Agent) (*Model, chan agent.Event) {
	t.Helper()
	m := New(Options{Agent: a, Cwd: "/work/project", Version: "test"})
	ch := m.events
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	return tm.(*Model), ch
}

// pump drains the agent's events into the model until the run finishes.
func pump(t *testing.T, m *Model, ch chan agent.Event) *Model {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			var tm tea.Model = m
			tm, _ = tm.Update(eventMsg(ev))
			m = tm.(*Model)
			if ev.Type == doneEventType {
				return m
			}
		case <-deadline:
			t.Fatal("timed out waiting for the run to finish")
		}
	}
}

func typeAndEnter(t *testing.T, m *Model, text string) (*Model, tea.Cmd) {
	t.Helper()
	var tm tea.Model = m
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
	tm, cmd := tm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return tm.(*Model), cmd
}

func TestSubmitRunsAgentAndRendersTranscript(t *testing.T) {
	fa := &fakeAgent{script: []agent.Event{
		{Type: agent.EventTextDelta, Text: "Sure, "},
		{Type: agent.EventTextDelta, Text: "creating the file."},
		{Type: agent.EventToolStart, ToolCall: &agent.ToolCall{ID: "1", Name: "write", Arguments: `{"path":"hello.txt","content":"hi"}`}},
		{Type: agent.EventToolResult, Result: &agent.ToolResult{Name: "write", Content: "wrote 2 bytes to hello.txt", DurationMS: 4}},
		{Type: agent.EventTurnEnd},
		{Type: agent.EventUsage, Usage: &agent.Usage{InputTokens: 1200, OutputTokens: 34}},
	}}
	m, ch := newTestModel(t, fa)

	if !strings.HasPrefix(m.View(), "hal test") {
		t.Fatalf("header missing from the view:\n%s", m.View())
	}

	m, cmd := typeAndEnter(t, m, "create hello.txt")
	if cmd == nil {
		t.Fatal("submitting should start the run and return a command")
	}
	if !m.running {
		t.Fatal("model should be marked as running")
	}

	m = pump(t, m, ch)

	if m.running {
		t.Fatal("running should be cleared once the run finishes")
	}
	if m.status != "ready" {
		t.Fatalf("status should return to ready, got %q", m.status)
	}
	if fa.promptCount() != 1 {
		t.Fatalf("expected exactly one prompt, got %d", fa.promptCount())
	}

	view := m.View()
	for _, want := range []string{"create hello.txt", "creating the file.", "write(hello.txt)", "wrote 2 bytes to hello.txt"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
	if !strings.Contains(view, "↑1.2k ↓34") {
		t.Fatalf("footer should show token usage:\n%s", view)
	}
	if !strings.Contains(view, "fake-model") {
		t.Fatalf("footer should show the model:\n%s", view)
	}
}

func TestToolResultErrorsAreMarked(t *testing.T) {
	fa := &fakeAgent{script: []agent.Event{
		{Type: agent.EventToolStart, ToolCall: &agent.ToolCall{ID: "1", Name: "bash", Arguments: `{"command":"exit 3"}`}},
		{Type: agent.EventToolResult, Result: &agent.ToolResult{Name: "bash", Content: "exit code 3", IsError: true}},
	}}
	m, _ := newTestModel(t, fa)
	m, _ = typeAndEnter(t, m, "run it")
	// fold the scripted events directly: deterministic, no goroutine race
	for _, ev := range fa.script {
		var tm tea.Model = m
		tm, _ = tm.Update(eventMsg(ev))
		m = tm.(*Model)
	}
	if len(m.blocks) < 2 {
		t.Fatalf("expected a tool block, got %d blocks", len(m.blocks))
	}
	tb := m.blocks[len(m.blocks)-1]
	if !tb.isErr || tb.kind != kindTool {
		t.Fatalf("tool failure not marked: %+v", tb)
	}
}

func TestEscAbortsRunningAgent(t *testing.T) {
	fa := &fakeAgent{blocking: true}
	m, ch := newTestModel(t, fa)

	m, _ = typeAndEnter(t, m, "do something slow")
	if !m.running {
		t.Fatal("expected the run to be active")
	}
	var tm tea.Model = m
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(*Model)
	if !strings.Contains(m.View(), "aborted by user") {
		t.Fatalf("abort notice missing:\n%s", m.View())
	}

	// the agent observes cancellation and the run completes
	m = pump(t, m, ch)
	if !fa.wasAborted() {
		t.Fatal("agent should have seen its context cancelled")
	}
	if m.running {
		t.Fatal("abort should clear the running state")
	}
	if !strings.Contains(m.View(), "ready") {
		t.Fatalf("status should return to ready:\n%s", m.View())
	}
}

func TestQueuedPromptRunsAfterCurrentTurn(t *testing.T) {
	fa := &fakeAgent{blocking: true}
	m, ch := newTestModel(t, fa)

	m, _ = typeAndEnter(t, m, "first task")
	m, _ = typeAndEnter(t, m, "second task")
	if len(m.queued) != 1 {
		t.Fatalf("expected one queued prompt, got %d", len(m.queued))
	}
	if !strings.Contains(m.View(), "queued") {
		t.Fatalf("queue notice missing:\n%s", m.View())
	}

	// abort the first run; the queued prompt should start automatically
	var tm tea.Model = m
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(*Model)
	m = pump(t, m, ch)

	// the queued run starts in its own goroutine
	deadline := time.Now().Add(2 * time.Second)
	for fa.promptCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fa.promptCount() != 2 {
		t.Fatalf("expected the queued prompt to run as well, got %d prompts", fa.promptCount())
	}
	if fa.prompts[1] != "second task" {
		t.Fatalf("wrong queued prompt ran: %q", fa.prompts[1])
	}
	if len(m.queued) != 0 {
		t.Fatalf("queue should be drained, got %+v", m.queued)
	}
	if strings.Contains(m.View(), "run failed") {
		t.Fatalf("a user abort must not be reported as a failure:\n%s", m.View())
	}
}

func TestSlashCommands(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)

	run := func(cmd string) *Model {
		var tm tea.Model = m
		tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(cmd)})
		tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyEnter})
		return tm.(*Model)
	}

	m = run("/help")
	if !strings.Contains(m.View(), "/model") || !strings.Contains(m.View(), "commands:") {
		t.Fatalf("/help output missing:\n%s", m.View())
	}
	if fa.promptCount() != 0 {
		t.Fatal("slash commands must not reach the agent")
	}

	m = run("/model sonnet")
	if !strings.Contains(m.View(), "switched to sonnet") {
		t.Fatalf("/model did not switch:\n%s", m.View())
	}
	if fa.ModelName() != "sonnet" {
		t.Fatalf("agent model not updated: %q", fa.ModelName())
	}

	m = run("/tools")
	if !strings.Contains(m.View(), "read, bash, edit, write") {
		t.Fatalf("/tools listing missing:\n%s", m.View())
	}

	sessionCalled := false
	m.opts.SessionInfo = func() string { sessionCalled = true; return "id deadbeef" }
	m = run("/session")
	if !sessionCalled || !strings.Contains(m.View(), "id deadbeef") {
		t.Fatalf("/session output missing:\n%s", m.View())
	}

	m = run("/nope")
	if !strings.Contains(m.View(), "unknown command") {
		t.Fatalf("unknown command should be reported:\n%s", m.View())
	}

	m = run("/clear")
	if len(m.blocks) != 0 {
		t.Fatalf("/clear should empty the transcript, got %d blocks", len(m.blocks))
	}
}

func TestNewCommandResetsConversation(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)
	newCalled := false
	m.opts.NewSession = func() (string, error) { newCalled = true; return "(session abc12345)", nil }

	m, _ = typeAndEnter(t, m, "hello")
	m, _ = typeAndEnter(t, m, "/new")

	if !newCalled {
		t.Fatal("NewSession callback should be invoked")
	}
	if len(m.blocks) != 1 || m.blocks[0].kind != kindSystem {
		t.Fatalf("expected a single system notice after /new, got %+v", m.blocks)
	}
	if !strings.Contains(m.blocks[0].body, "abc12345") {
		t.Fatalf("new session id not reported: %q", m.blocks[0].body)
	}
}

func TestRunFailureIsReported(t *testing.T) {
	fa := &fakeAgent{failWith: errors.New("provider exploded")}
	m, ch := newTestModel(t, fa)
	m, _ = typeAndEnter(t, m, "do it")
	m = pump(t, m, ch)

	view := m.View()
	if !strings.Contains(view, "provider exploded") {
		t.Fatalf("failure not shown:\n%s", view)
	}
	if m.running {
		t.Fatal("a failed run should clear the running state")
	}
}

func TestSummarizeArgs(t *testing.T) {
	cases := map[string]string{
		`{"path":"a/b.go"}`:                           "a/b.go",
		`{"command":"go test ./..."}`:                 "go test ./...",
		`{"pattern":"func main","path":"src"}`:        "src",
		`{"oldText":"x","newText":"y"}`:               `{"oldText":"x","newText":"y"}`,
		`not json`:                                    "not json",
		`{"path":"` + strings.Repeat("z", 200) + `"}`: "",
	}
	for raw, want := range cases {
		got := summarizeArgs(raw)
		if want == "" {
			if len(got) > 62 {
				t.Fatalf("long arg not truncated: %d chars", len(got))
			}
			continue
		}
		if got != want {
			t.Fatalf("summarizeArgs(%s) = %q, want %q", raw, got, want)
		}
	}
}
