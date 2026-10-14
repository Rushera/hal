// Package tui implements hal's interactive terminal interface with bubbletea:
// a header, a scrollable transcript of messages and tool calls, a multi-line
// editor, and a footer showing directory, model, session and token usage.
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Rushera/hal/internal/agent"
	"github.com/Rushera/hal/internal/session"
)

// Agent is the slice of *agent.Agent the TUI drives. Keeping it an interface
// makes the model unit-testable with a scripted fake.
type Agent interface {
	RunPrompt(ctx context.Context, prompt string, emit func(agent.Event)) error
	ModelName() string
	SetModelName(string) bool
	ProviderName() string
	Messages() []agent.Message
	Reset()
	ToolNames() []string
}

// Options configures the TUI.
type Options struct {
	Agent         Agent
	Cwd           string
	Version       string
	InitialPrompt string
	// SessionInfo describes the current session (/session command).
	SessionInfo func() string
	// SessionID reports the id of the current session, used to keep the
	// switcher from offering the session that is already showing.
	SessionID func() string
	// NewSession starts a fresh session file and returns its description.
	NewSession func() (string, error)
	// ListSessions reports the stored sessions that can be switched to.
	ListSessions func() []session.Info
	// OpenSession switches to a stored session by id and returns its
	// description.
	OpenSession func(id string) (string, error)
}

// Block kinds in the transcript.
const (
	kindUser      = "user"
	kindAssistant = "assistant"
	kindTool      = "tool"
	kindSystem    = "system"
	kindError     = "error"
)

const doneEventType = "__run_done"

type block struct {
	kind  string
	title string
	body  string
	done  bool
	isErr bool
}

// Model is the bubbletea model.
type Model struct {
	opts   Options
	vp     viewport.Model
	ta     textarea.Model
	sp     spinner.Model
	events chan agent.Event

	blocks  []block
	cur     int // index of the streaming assistant block, -1 if none
	curTool int

	running  bool
	aborting bool
	cancel   context.CancelFunc
	queued   []string
	width    int
	height   int
	ready    bool

	usageIn   int
	usageOut  int
	turns     int
	lastErr   error
	status    string
	startedAt time.Time

	// Session switcher state (/switch, /sessions).
	picking  bool
	choices  []session.Info
	selected int
}

type eventMsg agent.Event
type runDoneMsg struct{ err error }

// New builds the TUI model.
func New(opts Options) *Model {
	ta := textarea.New()
	ta.Placeholder = "Ask hal to do something…  (/help for commands, esc aborts)"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(3)
	ta.Prompt = "› "
	// Enter submits; newline needs an explicit modifier.
	ta.KeyMap.InsertNewline.SetKeys("alt+enter", "shift+enter", "ctrl+j")
	ta.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	m := &Model{
		opts:    opts,
		ta:      ta,
		sp:      sp,
		cur:     -1,
		curTool: -1,
		events:  make(chan agent.Event, 512),
		status:  "ready",
	}
	if opts.Version == "" {
		m.opts.Version = "dev"
	}
	m.addBlock(kindSystem, "", startupText(opts))
	return m
}

func startupText(opts Options) string {
	var b strings.Builder
	b.WriteString(headerLine(opts))
	if opts.Agent != nil {
		tools := opts.Agent.ToolNames()
		b.WriteString("\ntools: " + strings.Join(tools, ", "))
	}
	b.WriteString("\nenter submit · alt+enter newline · esc abort · pgup/pgdn scroll · ctrl+c quit")
	b.WriteString("\n/help /model /new /session /switch /sessions /tools /clear /quit")
	return b.String()
}

func headerLine(opts Options) string {
	model := "no model"
	provider := ""
	if opts.Agent != nil {
		if opts.Agent.ModelName() != "" {
			model = opts.Agent.ModelName()
		}
		provider = opts.Agent.ProviderName()
	}
	if provider != "" {
		return fmt.Sprintf("hal %s — %s (%s)", opts.Version, model, provider)
	}
	return fmt.Sprintf("hal %s — %s", opts.Version, model)
}

// Init implements tea.Model. An initial prompt supplied on the command line
// starts the first run here.
func (m *Model) Init() tea.Cmd {
	if strings.TrimSpace(m.opts.InitialPrompt) != "" {
		prompt := m.opts.InitialPrompt
		m.opts.InitialPrompt = ""
		m.addBlock(kindUser, "", prompt)
		return m.startRun(prompt)
	}
	return nil
}

func waitForEvent(ch <-chan agent.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return runDoneMsg{}
		}
		return eventMsg(ev)
	}
}

func (m *Model) startRun(prompt string) tea.Cmd {
	m.running = true
	m.startedAt = time.Now()
	m.status = "working"
	m.lastErr = nil
	m.cur = -1
	m.curTool = -1
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	ch := m.events
	ag := m.opts.Agent
	p := prompt
	// The turn belongs to the current conversation: remember which one, so a
	// prompt queued while it runs resumes the right session (after a /new the
	// queue is emptied to keep the turns of one conversation together).
	sid := m.sessionID()
	go func() {
		var err error
		if ag == nil {
			err = fmt.Errorf("no agent configured")
		} else {
			err = ag.RunPrompt(ctx, p, func(ev agent.Event) {
				select {
				case ch <- ev:
				case <-ctx.Done():
				}
			})
		}
		var msg agent.Event
		if sid != m.sessionID() {
			// The session was switched while this turn ran: its result belongs
			// to a conversation the console has left, so only unblock the UI.
			err = nil
			msg = agent.Event{Type: doneEventType}
		} else {
			msg = agent.Event{Type: doneEventType}
			if err != nil {
				msg.Error = err.Error()
			}
		}
		select {
		case ch <- msg:
		case <-time.After(2 * time.Second):
		}
	}()
	return tea.Batch(waitForEvent(ch), m.sp.Tick)
}

// sessionID reports the id of the session the console is showing, or "" when
// sessions are disabled.
func (m *Model) sessionID() string {
	if m.opts.SessionID == nil {
		return ""
	}
	return m.opts.SessionID()
}

// Update implements tea.Model. All Model methods use pointer receivers so the
// value returned to bubbletea is always the same *Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		m.ready = true
		m.refresh()
		return m, nil

	case spinner.TickMsg:
		if !m.running {
			return m, nil
		}
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd

	case eventMsg:
		ev := agent.Event(msg)
		if ev.Type == doneEventType {
			var err error
			if ev.Error != "" {
				err = errors.New(ev.Error)
			}
			return m.finishRun(err)
		}
		m.handleEvent(ev)
		return m, waitForEvent(m.events)

	case runDoneMsg:
		return m.finishRun(msg.err)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.picking {
		return m.handlePickerKey(msg)
	}
	switch msg.String() {
	case "ctrl+c":
		if m.running && m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
	case "esc":
		if m.running {
			if m.cancel != nil {
				m.cancel()
			}
			m.aborting = true
			m.status = "aborting"
			m.addBlock(kindSystem, "", "aborted by user")
			m.refresh()
			return m, nil
		}
		m.ta.SetValue("")
		return m, nil
	case "ctrl+l":
		m.blocks = nil
		m.refresh()
		return m, nil
	case "pgup", "pgdown", "alt+up", "alt+down":
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	case "enter":
		text := strings.TrimSpace(m.ta.Value())
		if text == "" {
			return m, nil
		}
		m.ta.Reset()
		if strings.HasPrefix(text, "/") {
			m.handleCommand(text)
			m.layout()
			m.refresh()
			return m, nil
		}
		if m.running {
			m.queued = append(m.queued, text)
			m.addBlock(kindSystem, "", "queued (runs after the current turn): "+firstLine(text))
			m.refresh()
			return m, nil
		}
		// Switching sessions while the agent works aborts the turn, so the run
		// finishing may still have a queue from the old conversation; this
		// prompt is dropped to keep the turns of one conversation together.
		if len(m.queued) > 0 {
			m.addBlock(kindSystem, "", "dropped (a session switch interrupted the queue): "+firstLine(text))
			m.refresh()
			return m, nil
		}
		m.addBlock(kindUser, "", text)
		m.refresh()
		return m, m.startRun(text)
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

// handlePickerKey drives the session switcher: navigation, selection and cancel.
func (m *Model) handlePickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k", "shift+tab":
		if len(m.choices) > 0 {
			m.selected = (m.selected - 1 + len(m.choices)) % len(m.choices)
		}
	case "down", "j", "tab":
		if len(m.choices) > 0 {
			m.selected = (m.selected + 1) % len(m.choices)
		}
	case "enter":
		m.pickSession()
	case "esc":
		m.picking = false
	case "ctrl+c":
		return m, tea.Quit
	default:
		// A digit jumps straight to the matching entry.
		if len(msg.Runes) == 1 && msg.Runes[0] >= '1' && msg.Runes[0] <= '9' {
			if i := int(msg.Runes[0] - '1'); i < len(m.choices) {
				m.selected = i
				m.pickSession()
			}
		}
	}
	m.layout()
	m.refresh()
	return m, nil
}

// openPicker collects the stored sessions and shows the switcher.
func (m *Model) openPicker() {
	if m.opts.ListSessions == nil || m.opts.OpenSession == nil {
		m.addBlock(kindSystem, "sessions", "sessions are disabled (--no-session)")
		return
	}
	// A session switch aborts the turn in flight; the queue from the old
	// conversation goes with it. Marking it as an abort keeps the picker from
	// closing on a "run failed" notice the cancelled turn would otherwise add.
	if m.running && m.cancel != nil {
		m.aborting = true
		m.cancel()
		m.queued = nil
	}
	infos := m.opts.ListSessions()
	current := m.sessionID()
	choices := make([]session.Info, 0, len(infos))
	for _, info := range infos {
		if info.ID == current {
			continue // already showing it; an empty list keeps the picker hidden
		}
		choices = append(choices, info)
	}
	m.choices = choices
	m.selected = 0
	if len(choices) == 0 {
		if len(infos) == 0 {
			m.addBlock(kindSystem, "sessions", "no other sessions stored for this directory")
		} else {
			m.addBlock(kindSystem, "sessions", "no other sessions stored (this is the only one)")
		}
		return
	}
	m.picking = true
}

// pickSession switches to the highlighted session.
func (m *Model) pickSession() {
	if m.selected < 0 || m.selected >= len(m.choices) {
		return
	}
	id := m.choices[m.selected].ID
	m.picking = false
	m.choices = nil
	m.switchTo(id)
}

// switchTo opens the stored session with the given id (or id prefix) and
// replays its conversation into the transcript.
func (m *Model) switchTo(id string) {
	if m.opts.OpenSession == nil {
		m.addBlock(kindSystem, "sessions", "sessions are disabled (--no-session)")
		return
	}
	// A switch aborts the turn in flight; its queue belonged to the session
	// that is being left. It is marked as an abort so the run's cancellation
	// is not reported as a failure of the session being opened.
	if m.running && m.cancel != nil {
		m.aborting = true
		m.cancel()
		m.queued = nil
	}
	desc, err := m.opts.OpenSession(id)
	if err != nil {
		m.addBlock(kindError, "switch session", err.Error())
		return
	}
	if desc != "" {
		desc = " " + desc
	}
	m.blocks = nil
	m.turns = 0
	m.usageIn, m.usageOut = 0, 0
	m.cur, m.curTool = -1, -1
	m.lastErr = nil
	m.replayHistory()
	m.addBlock(kindSystem, "switched session", "showing session "+shortID(id)+desc)
}

// shortID renders a session id the way the file names and listings do.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// finishRun clears the working state, reports a failure, and starts the next
// queued prompt if there is one.
func (m *Model) finishRun(err error) (tea.Model, tea.Cmd) {
	// The agent reports its error as text over the event channel, so a
	// stringified "context canceled" no longer matches errors.Is. Track the
	// abort locally instead of re-deriving cancellation from the message.
	aborted := m.aborting
	m.aborting = false
	m.running = false
	m.cur = -1
	m.curTool = -1
	m.cancel = nil
	if err != nil && !aborted && !errors.Is(err, context.Canceled) && m.lastErr == nil {
		m.lastErr = err
		m.addBlock(kindError, "run failed", err.Error())
	}
	if len(m.queued) > 0 {
		next := m.queued[0]
		m.queued = m.queued[1:]
		m.refresh()
		return m, m.startRun(next)
	}
	m.status = "ready"
	m.refresh()
	return m, nil
}

// replayHistory rebuilds the transcript from the conversation the agent holds,
// which is what makes a session switch show the conversation it switched to.
// Live tool results are unavailable from the history alone, so each tool call
// is one block; the next turn streams on top of them.
func (m *Model) replayHistory() {
	if m.opts.Agent == nil {
		return
	}
	msgs := m.opts.Agent.Messages()
	if len(msgs) == 0 {
		m.addBlock(kindSystem, "", "(this session has no messages yet)")
		return
	}
	restored := 0
	for _, msg := range msgs {
		switch msg.Role {
		case agent.RoleUser:
			if strings.TrimSpace(msg.Content) == "" {
				continue
			}
			m.addBlock(kindUser, "", msg.Content)
		case agent.RoleAssistant:
			if strings.TrimSpace(msg.Content) != "" {
				m.addBlock(kindAssistant, "", msg.Content)
			}
			for _, call := range msg.ToolCalls {
				m.addBlock(kindTool, call.Name+"("+summarizeArgs(call.Arguments)+")", "")
				restored++
			}
		}
	}
	if restored > 0 {
		m.addBlock(kindSystem, "", fmt.Sprintf("restored %d messages (tool output is not shown)", len(msgs)))
	}
}

// handleEvent folds one agent event into the transcript.
func (m *Model) handleEvent(ev agent.Event) {
	switch ev.Type {
	case agent.EventTextDelta:
		if m.cur < 0 {
			m.addBlock(kindAssistant, "", "")
			m.cur = len(m.blocks) - 1
		}
		m.blocks[m.cur].body += ev.Text
	case agent.EventToolStart:
		if ev.ToolCall != nil {
			title := ev.ToolCall.Name + "(" + summarizeArgs(ev.ToolCall.Arguments) + ")"
			m.addBlock(kindTool, title, "")
			m.curTool = len(m.blocks) - 1
		}
		m.cur = -1
	case agent.EventToolResult:
		if ev.Result != nil && m.curTool >= 0 && m.curTool < len(m.blocks) {
			b := &m.blocks[m.curTool]
			b.body = ev.Result.Content
			b.done = true
			b.isErr = ev.Result.IsError
			if ev.Result.DurationMS > 0 {
				b.title = fmt.Sprintf("%s  [%dms]", b.title, ev.Result.DurationMS)
			}
		}
		m.curTool = -1
		m.cur = -1
	case agent.EventTurnEnd:
		m.cur = -1
		m.turns++
	case agent.EventUsage:
		if ev.Usage != nil {
			m.usageIn += ev.Usage.InputTokens
			m.usageOut += ev.Usage.OutputTokens
		}
	case agent.EventError:
		m.lastErr = fmt.Errorf("%s", ev.Error)
		m.addBlock(kindError, "error", ev.Error)
	case agent.EventRetry:
		m.addBlock(kindSystem, "retrying",
			fmt.Sprintf("%s — attempt %d of %d in %.1fs", ev.Error, ev.Attempt, ev.MaxAttempts, float64(ev.RetryInMS)/1000))
	}
	m.refresh()
}

func (m *Model) handleCommand(text string) {
	fields := strings.Fields(text)
	cmd := strings.ToLower(strings.TrimPrefix(fields[0], "/"))
	args := fields[1:]
	switch cmd {
	case "help", "?":
		m.addBlock(kindSystem, "help", helpText())
	case "model":
		if len(args) == 0 {
			m.addBlock(kindSystem, "model", "current model: "+m.opts.Agent.ModelName())
			return
		}
		name := strings.Join(args, " ")
		if m.opts.Agent.SetModelName(name) {
			m.addBlock(kindSystem, "model", "switched to "+name)
			if m.opts.SessionInfo != nil {
				// session model bookkeeping is the caller's business
				_ = m.opts.SessionInfo
			}
		} else {
			m.addBlock(kindError, "model", "this provider cannot switch models at runtime")
		}
	case "new":
		m.opts.Agent.Reset()
		m.blocks = nil
		m.turns = 0
		m.usageIn, m.usageOut = 0, 0
		m.queued = nil
		m.cur, m.curTool = -1, -1
		m.lastErr = nil
		desc := ""
		if m.opts.NewSession != nil {
			if d, err := m.opts.NewSession(); err != nil {
				m.addBlock(kindError, "new session", err.Error())
			} else {
				desc = d
			}
		}
		m.addBlock(kindSystem, "new session", "conversation cleared "+desc)
	case "session":
		info := "no session (ephemeral mode)"
		if m.opts.SessionInfo != nil {
			info = m.opts.SessionInfo()
		}
		m.addBlock(kindSystem, "session", info)
	case "switch", "sessions", "resume":
		if len(args) > 0 {
			m.switchTo(args[0])
		} else {
			m.openPicker()
		}
		m.layout()
	case "tools":
		m.addBlock(kindSystem, "tools", strings.Join(m.opts.Agent.ToolNames(), ", "))
	case "clear":
		m.blocks = nil
	case "quit", "exit":
		m.addBlock(kindSystem, "", "use ctrl+c to quit")
	default:
		m.addBlock(kindError, "", "unknown command: /"+cmd+" (try /help)")
	}
}

func helpText() string {
	return `commands:
  /help              show this help
  /model [name]      show or switch the model
  /new               start a new session
  /session           show the current session
  /switch [id]       switch to a stored session (no id opens a picker)
  /sessions          list the stored sessions for this directory
  /tools             list enabled tools
  /clear             clear the transcript (ctrl+l)
  /quit              quit (ctrl+c)
keys:
  enter              submit (queues while the agent is working)
  alt+enter          newline
  esc                abort the current run, or cancel the switcher
  ↑/↓                choose a session in the switcher (enter opens it)
  pgup/pgdn          scroll the transcript
  ctrl+c             quit`
}

func (m *Model) addBlock(kind, title, body string) {
	m.blocks = append(m.blocks, block{kind: kind, title: title, body: body, done: true})
	m.cur = -1
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 60)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// summarizeArgs renders tool arguments as a short one-line summary.
func summarizeArgs(raw string) string {
	var generic map[string]any
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		return truncate(strings.ReplaceAll(raw, "\n", " "), 60)
	}
	for _, key := range []string{"path", "command", "pattern", "query"} {
		if v, ok := generic[key].(string); ok && v != "" {
			return truncate(strings.ReplaceAll(v, "\n", " "), 60)
		}
	}
	return truncate(strings.ReplaceAll(raw, "\n", " "), 60)
}

// pickerWindow is how many sessions fit in the picker, given the terminal
// height. A picker that does not fit would push the footer off the screen.
func (m *Model) pickerWindow() int {
	// Header (1) + picker border (2) + heading (1) + summary (1) + footer (1).
	room := m.height - 6
	if room > pickerMax {
		room = pickerMax
	}
	if room < 1 {
		room = 1
	}
	return room
}

// pickerLines counts what viewPicker renders, for layout.
func (m *Model) pickerLines() int {
	start, end := m.pickerRange()
	lines := 1 + (end - start) + 2 // heading + entries + border
	if start > 0 || end < len(m.choices) {
		lines++
	}
	return lines
}

// pickerRange is the slice of choices the window shows.
func (m *Model) pickerRange() (start, end int) {
	window := m.pickerWindow()
	if m.selected >= window {
		start = m.selected - window + 1
	}
	end = start + window
	if end > len(m.choices) {
		end = len(m.choices)
	}
	return start, end
}

func (m *Model) layout() {
	editorHeight := m.ta.Height() + 2 // textarea + border
	avail := m.height - editorHeight - 3
	if m.picking {
		// While picking, the transcript steps aside: the picker needs the room
		// and a short transcript viewport is a small price for a list that fits.
		avail = 1
	}
	if avail < 1 {
		avail = 1
	}
	if !m.ready || m.vp.Width == 0 {
		m.vp = viewport.New(m.width, avail)
	} else {
		m.vp.Width = m.width
		m.vp.Height = avail
	}
	m.ta.SetWidth(m.width - 4)
}

// refresh re-renders the transcript into the viewport.
func (m *Model) refresh() {
	if !m.ready {
		return
	}
	content := m.renderBlocks()
	m.vp.SetContent(content)
	m.vp.GotoBottom()
}

// View implements tea.Model.
func (m *Model) View() string {
	if !m.ready {
		return "starting hal…"
	}
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Render(
		fmt.Sprintf("hal %s", m.opts.Version)))
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(
		"  " + shortenHome(m.opts.Cwd)))
	b.WriteString("\n")
	if m.picking {
		// The picker replaces the transcript: it needs the vertical room, and
		// a wrapped list would overflow the terminal.
		b.WriteString(m.viewPicker())
	} else {
		b.WriteString(m.vp.View())
		b.WriteString("\n")
		b.WriteString(m.viewEditor())
	}
	b.WriteString("\n")
	b.WriteString(m.footer())
	return b.String()
}

// viewEditor renders the input box, coloured while a turn is running.
func (m Model) viewEditor() string {
	borderColor := "240"
	if m.running {
		borderColor = "205"
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(borderColor)).
		Width(m.width - 2).
		Render(m.ta.View())
}

// pickerMax is how many sessions are shown at once; the list scrolls when the
// directory has more.
const pickerMax = 8

// viewPicker renders the session switcher in place of the editor.
func (m Model) viewPicker() string {
	// The border and its padding take four columns; entries are clipped to what
	// is left, otherwise a narrow terminal wraps them and the box overflows.
	inner := m.width - 4
	if inner < 10 {
		inner = 10
	}
	clip := func(s string) string {
		r := []rune(s)
		if len(r) <= inner {
			return s
		}
		if inner <= 1 {
			return string(r[:inner])
		}
		return string(r[:inner-1]) + "…"
	}

	lines := []string{lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).
		Render(clip("switch session  (↑/↓ choose · enter open · esc cancel)"))}

	start, end := m.pickerRange()
	current := m.sessionID()
	for i := start; i < end; i++ {
		info := m.choices[i]
		text := sessionLabel(info)
		if info.ID == current {
			text += "  (current)"
		}
		if i == m.selected {
			lines = append(lines, lipgloss.NewStyle().
				Bold(true).Foreground(lipgloss.Color("39")).
				Render(clip("› "+text)))
		} else {
			lines = append(lines, clip("  "+text))
		}
	}
	if start > 0 || end < len(m.choices) {
		lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color("240")).
			Render(clip(fmt.Sprintf("  %d–%d of %d", start+1, end, len(m.choices)))))
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("205")).
		Width(m.width - 2).
		Render(strings.Join(lines, "\n"))
}

// sessionLabel describes a stored session in one line: its short id, why it is
// recognisable, and how much conversation it holds.
func sessionLabel(info session.Info) string {
	name := strings.TrimSpace(info.Name)
	short := shortID(info.ID)
	if short == "" {
		short = "?"
	}
	body := fmt.Sprintf("%s · %d msgs · %s",
		info.Model, info.Messages, info.UpdatedAt.Format("01-02 15:04"))
	if name == "" || name == short {
		return fmt.Sprintf("%s  %s", short, body)
	}
	return fmt.Sprintf("%s  %s · %s", short, name, body)
}

func (m Model) footer() string {
	left := shortenHome(m.opts.Cwd)
	model := "no model"
	if m.opts.Agent != nil && m.opts.Agent.ModelName() != "" {
		model = m.opts.Agent.ModelName()
	}
	status := m.status
	if m.running {
		status = m.sp.View() + " working"
	}
	right := fmt.Sprintf("%s · %s · ↑%s ↓%s · %d turns",
		model, status, humanCount(m.usageIn), humanCount(m.usageOut), m.turns)
	width := m.width
	if width <= 0 {
		width = 80
	}
	gap := width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("240")).
		Render(left + strings.Repeat(" ", gap) + right)
}

func (m Model) renderBlocks() string {
	if len(m.blocks) == 0 {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("(empty conversation — type a task)")
	}
	var parts []string
	for _, b := range m.blocks {
		parts = append(parts, m.renderBlock(b))
	}
	return strings.Join(parts, "\n")
}

func (m Model) renderBlock(b block) string {
	width := m.width - 4
	if width < 20 {
		width = 20
	}
	switch b.kind {
	case kindUser:
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39")).
			Width(width).Render("› " + b.body)
	case kindAssistant:
		out := lipgloss.NewStyle().Width(width).Render(b.body)
		if !b.done && b.body == "" {
			out = m.sp.View() + " thinking…"
		}
		return out
	case kindTool:
		name := lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("⚙ " + b.title)
		body := preview(b.body)
		if body == "" {
			body = "(running…)"
		}
		style := lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Width(width)
		if b.isErr {
			style = style.Foreground(lipgloss.Color("203"))
		}
		return name + "\n" + style.Render(body)
	case kindError:
		head := "✗ " + b.title
		if b.title == "" {
			head = "✗ error"
		}
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Width(width).
			Render(head + "\n" + b.body)
	default:
		head := b.title
		if head != "" {
			head += "\n"
		}
		return lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Width(width).
			Render(head + b.body)
	}
}

// preview limits a tool result to its first lines plus a summary of the rest.
func preview(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	const maxLines = 12
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[:maxLines], "\n") +
		fmt.Sprintf("\n… (%d more lines)", len(lines)-maxLines)
}

func shortenHome(p string) string {
	if home, err := homeDir(); err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func humanCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
