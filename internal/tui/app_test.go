package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Rushera/hal/internal/agent"
	"github.com/Rushera/hal/internal/session"
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
	history  []agent.Message
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
func (f *fakeAgent) ToolNames() []string        { return []string{"read", "bash", "edit", "write"} }

func (f *fakeAgent) Messages() []agent.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.Message(nil), f.history...)
}

func (f *fakeAgent) Reset() { f.mu.Lock(); f.history = nil; f.mu.Unlock() }

func (f *fakeAgent) setHistory(msgs ...agent.Message) {
	f.mu.Lock()
	f.history = msgs
	f.mu.Unlock()
}

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

// sessionInfo builds a stored-session entry for the switcher tests.
func sessionEntry(id, name string) session.Info {
	return session.Info{
		ID: id, Name: name, Model: "fake-model", Messages: 2,
		UpdatedAt: time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC),
	}
}

// withSessions wires the session callbacks the switcher uses onto a test model.
func withSessions(m *Model, current string, infos ...session.Info) *Model {
	m.opts.SessionID = func() string { return current }
	m.opts.SessionInfo = func() string { return "id " + current }
	m.opts.ListSessions = func() []session.Info { return infos }
	m.opts.OpenSession = func(id string) (string, error) {
		current = id
		return "(session " + id[:8] + " · 1 messages)", nil
	}
	return m
}

func press(t *testing.T, m *Model, key string) *Model {
	t.Helper()
	var tm tea.Model = m
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
	return tm.(*Model)
}

func pressKey(t *testing.T, m *Model, k tea.KeyType) *Model {
	t.Helper()
	var tm tea.Model = m
	tm, _ = tm.Update(tea.KeyMsg{Type: k})
	return tm.(*Model)
}

func TestSwitchCommandOpensThePickerAndSwitches(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)
	m = withSessions(m, "aaaa000011112222", sessionEntry("bbbb111122223333", "older task"))
	fa.setHistory(agent.NewUserMessage("from bbbb"))

	m, _ = typeAndEnter(t, m, "/switch")
	if !m.picking {
		t.Fatalf("/switch should open the picker:\n%s", m.View())
	}
	view := m.View()
	if !strings.Contains(view, "switch session") || !strings.Contains(view, "bbbb1111") {
		t.Fatalf("picker not rendered:\n%s", view)
	}
	if !strings.Contains(view, "older task") {
		t.Fatalf("picker should show the stored name:\n%s", view)
	}

	m = pressKey(t, m, tea.KeyEnter)
	if m.picking {
		t.Fatal("picking should end after a selection")
	}
	if got := m.sessionID(); got != "bbbb111122223333" {
		t.Fatalf("session not switched: %q", got)
	}
	view = m.View()
	if !strings.Contains(view, "from bbbb") {
		t.Fatalf("switched conversation not replayed:\n%s", view)
	}
	if !strings.Contains(view, "switched session") {
		t.Fatalf("switch not reported:\n%s", view)
	}
}

func TestPickerNavigatesAndCancels(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)
	m = withSessions(m, "current0", sessionEntry("aaaa000011112222", "one"),
		sessionEntry("bbbb111122223333", "two"), sessionEntry("cccc222233334444", "three"))

	m, _ = typeAndEnter(t, m, "/sessions")
	if !m.picking {
		t.Fatal("/sessions without a stored list should still open the picker")
	}
	if m.selected != 0 {
		t.Fatalf("selected = %d, want the first entry", m.selected)
	}
	m = press(t, m, "j")
	if m.selected != 1 {
		t.Fatalf("j should move down, selected = %d", m.selected)
	}
	m = pressKey(t, m, tea.KeyUp)
	if m.selected != 0 {
		t.Fatalf("up should move back, selected = %d", m.selected)
	}
	// wrap-around keeps the list navigable
	m = pressKey(t, m, tea.KeyUp)
	if m.selected != 2 {
		t.Fatalf("up from the first entry should wrap, selected = %d", m.selected)
	}

	m = pressKey(t, m, tea.KeyEsc)
	if m.picking {
		t.Fatal("esc should close the picker")
	}
	if got := m.sessionID(); got != "current0" {
		t.Fatalf("cancelling must not switch sessions: %q", got)
	}
}

func TestSwitchToIDReplaysTheStoredConversation(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)
	m = withSessions(m, "aaaa000011112222", sessionEntry("bbbb111122223333", "older"))
	fa.setHistory(
		agent.NewUserMessage("list the files"),
		agent.Message{Role: agent.RoleAssistant, Content: "here you go", ToolCalls: []agent.ToolCall{
			{ID: "1", Name: "ls", Arguments: `{"path":"."}`},
		}},
		agent.Message{Role: agent.RoleTool, ToolCallID: "1", Content: "main.go"},
		agent.Message{Role: agent.RoleAssistant, Content: "done"},
	)

	m, _ = typeAndEnter(t, m, "/switch bbbb1111")
	if m.picking {
		t.Fatal("/switch with an id should switch directly, not open the picker")
	}
	view := m.View()
	for _, want := range []string{"list the files", "here you go", "done", "ls(.)", "switched session"} {
		if !strings.Contains(view, want) {
			t.Fatalf("switched transcript missing %q:\n%s", want, view)
		}
	}
	if m.turns != 0 || m.usageIn != 0 || m.usageOut != 0 {
		t.Fatalf("the new session's counters should start at zero: %d %d %d", m.turns, m.usageIn, m.usageOut)
	}
}

func TestSwitchReportsOpenFailures(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)
	m.opts.SessionID = func() string { return "aaaa000011112222" }
	m.opts.OpenSession = func(string) (string, error) { return "", errors.New("no such session") }

	m, _ = typeAndEnter(t, m, "/switch deadbeef")
	if !strings.Contains(m.View(), "no such session") {
		t.Fatalf("the failure should be reported:\n%s", m.View())
	}
}

func TestPickerSkipsTheCurrentSessionAndSaysWhenThereIsNothingElse(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)
	m = withSessions(m, "aaaa000011112222", sessionEntry("aaaa000011112222", "current"))

	m, _ = typeAndEnter(t, m, "/switch")
	if m.picking {
		t.Fatal("a picker with no other session should not open")
	}
	if !strings.Contains(m.View(), "no other sessions") {
		t.Fatalf("the empty picker should say so:\n%s", m.View())
	}
}

func TestSwitchWithoutSessionCallbacksIsReported(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)

	m, _ = typeAndEnter(t, m, "/switch")
	if m.picking {
		t.Fatal("the picker must not open when sessions are disabled")
	}
	if !strings.Contains(m.View(), "--no-session") {
		t.Fatalf("disabled sessions should be explained:\n%s", m.View())
	}
}

func TestNewCommandDropsQueuedAndRepaintsTheEditor(t *testing.T) {
	fa := &fakeAgent{blocking: true}
	m, ch := newTestModel(t, fa)

	m, _ = typeAndEnter(t, m, "first")
	m, _ = typeAndEnter(t, m, "second")
	if len(m.queued) != 1 {
		t.Fatalf("queued = %v, want the second prompt", m.queued)
	}

	m, _ = typeAndEnter(t, m, "/new")
	if len(m.queued) != 0 {
		t.Fatalf("/new should drop the queue, got %v", m.queued)
	}
	if !strings.Contains(m.View(), "› ") {
		t.Fatalf("the editor should come back after /new:\n%s", m.View())
	}
	if fa.wasAborted() {
		t.Fatal("starting a session must not abort the running turn")
	}
	// drain the run so the test does not leave a goroutine blocked
	if m.cancel != nil {
		m.cancel()
	}
	pump(t, m, ch)
}

func TestRetryIsShownInTheTranscript(t *testing.T) {
	fa := &fakeAgent{script: []agent.Event{
		{Type: agent.EventRetry, Error: "read: connection reset by peer",
			Attempt: 2, MaxAttempts: 4, RetryInMS: 1500},
		{Type: agent.EventTextDelta, Text: "recovered"},
		{Type: agent.EventDone},
	}}
	m, ch := newTestModel(t, fa)
	m, _ = typeAndEnter(t, m, "do it")
	m = pump(t, m, ch)

	view := m.View()
	if !strings.Contains(view, "retrying") || !strings.Contains(view, "connection reset") {
		t.Fatalf("the retry should be shown:\n%s", view)
	}
	if !strings.Contains(view, "attempt 2 of 4") {
		t.Fatalf("the attempt count should be shown:\n%s", view)
	}
	if strings.Contains(view, "run failed") {
		t.Fatalf("a retry is not a failure:\n%s", view)
	}
	if m.lastErr != nil {
		t.Fatalf("a retry must not be recorded as the last error: %v", m.lastErr)
	}
}

func TestSwitchingWhileRunningIsNotReportedAsAFailure(t *testing.T) {
	fa := &fakeAgent{blocking: true}
	m, ch := newTestModel(t, fa)
	m.opts.SessionID = func() string { return "aaaa000011112222" }
	m.opts.OpenSession = func(id string) (string, error) { return "(session " + id[:8] + ")", nil }

	m, _ = typeAndEnter(t, m, "long task")
	m, _ = typeAndEnter(t, m, "/switch bbbb1111")
	m = pump(t, m, ch)

	view := m.View()
	if strings.Contains(view, "run failed") {
		t.Fatalf("the abort a switch causes must not be reported as a failure:\n%s", view)
	}
	if !strings.Contains(view, "switched session") {
		t.Fatalf("the switch should still be reported:\n%s", view)
	}
}

func TestPickerScrollsAndKeepsItsLayout(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)
	var infos []session.Info
	for i := 0; i < pickerMax+5; i++ {
		infos = append(infos, sessionEntry(fmt.Sprintf("id%011d", i), fmt.Sprintf("task %d", i)))
	}
	m = withSessions(m, "current0", infos...)

	m, _ = typeAndEnter(t, m, "/switch")
	if !m.picking {
		t.Fatal("the picker should open with many sessions")
	}
	// A window is shown, not the whole list.
	view := m.View()
	if strings.Contains(view, "task 12") {
		t.Fatalf("the picker should not show every entry:\n%s", view)
	}
	if !strings.Contains(view, "task 0") {
		t.Fatalf("the first entries should be visible:\n%s", view)
	}
	if !strings.Contains(view, fmt.Sprintf("1–%d of %d", pickerMax, len(infos))) {
		t.Fatalf("the window should be summarised:\n%s", view)
	}

	// Walk to the end; the window must follow the selection and stay bounded.
	for i := 0; i < len(infos)-1; i++ {
		m = press(t, m, "j")
	}
	if m.selected != len(infos)-1 {
		t.Fatalf("selected = %d, want the last entry", m.selected)
	}
	view = m.View()
	if !strings.Contains(view, fmt.Sprintf("task %d", len(infos)-1)) {
		t.Fatalf("the selected entry should be visible:\n%s", view)
	}
	if !strings.Contains(view, fmt.Sprintf("%d–%d of %d", len(infos)-pickerMax+1, len(infos), len(infos))) {
		t.Fatalf("the window should have scrolled:\n%s", view)
	}

	// The layout reserves room for the picker: it must never exceed the terminal.
	lines := strings.Count(m.View(), "\n") + 1
	if lines > 40 {
		t.Fatalf("the view overflowed the terminal (%d lines):\n%s", lines, m.View())
	}
}

func TestPickerDigitShortcutSelects(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)
	m = withSessions(m, "current0",
		sessionEntry("aaaa000011112222", "one"),
		sessionEntry("bbbb111122223333", "two"),
		sessionEntry("cccc222233334444", "three"))

	m, _ = typeAndEnter(t, m, "/switch")
	m = press(t, m, "3")
	if m.picking {
		t.Fatal("a digit should select immediately")
	}
	if got := m.sessionID(); got != "cccc222233334444" {
		t.Fatalf("digit 3 should pick the third entry, got %q", got)
	}
}

func TestPickerIgnoresDigitsPastTheList(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)
	m = withSessions(m, "current0", sessionEntry("aaaa000011112222", "one"))

	m, _ = typeAndEnter(t, m, "/switch")
	m = press(t, m, "9")
	if !m.picking {
		t.Fatal("a digit past the end must not select anything")
	}
	if got := m.sessionID(); got != "current0" {
		t.Fatalf("session changed to %q", got)
	}
}

func TestPickerAtASmallTerminalStaysUsable(t *testing.T) {
	fa := &fakeAgent{}
	m := New(Options{Agent: fa, Cwd: "/work", Version: "test"})
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m = tm.(*Model)
	var infos []session.Info
	for i := 0; i < pickerMax+3; i++ {
		infos = append(infos, sessionEntry(fmt.Sprintf("id%011d", i), fmt.Sprintf("t%d", i)))
	}
	m = withSessions(m, "current0", infos...)

	m, _ = typeAndEnter(t, m, "/switch")
	view := m.View()
	if !strings.Contains(view, "switch session") {
		t.Fatalf("the picker should render on a small terminal:\n%s", view)
	}
	if lines := strings.Count(view, "\n") + 1; lines > 12 {
		t.Fatalf("the view overflowed a 12-line terminal (%d lines):\n%s", lines, view)
	}
}
