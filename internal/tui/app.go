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
	// NewSession starts a fresh session file and returns its description.
	NewSession func() (string, error)
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
	b.WriteString("\n/help /model /new /session /tools /clear /quit")
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
		msg := agent.Event{Type: doneEventType}
		if err != nil {
			msg.Error = err.Error()
		}
		select {
		case ch <- msg:
		case <-time.After(2 * time.Second):
		}
	}()
	return tea.Batch(waitForEvent(ch), m.sp.Tick)
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
			m.refresh()
			return m, nil
		}
		if m.running {
			m.queued = append(m.queued, text)
			m.addBlock(kindSystem, "", "queued (runs after the current turn): "+firstLine(text))
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
  /tools             list enabled tools
  /clear             clear the transcript (ctrl+l)
  /quit              quit (ctrl+c)
keys:
  enter              submit (queues while the agent is working)
  alt+enter          newline
  esc                abort the current run
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

func (m *Model) layout() {
	editorHeight := m.ta.Height() + 2 // textarea + border
	avail := m.height - editorHeight - 3
	if avail < 3 {
		avail = 3
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
	b.WriteString(m.vp.View())
	b.WriteString("\n")
	borderColor := "240"
	if m.running {
		borderColor = "205"
	}
	b.WriteString(lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(borderColor)).
		Width(m.width - 2).
		Render(m.ta.View()))
	b.WriteString("\n")
	b.WriteString(m.footer())
	return b.String()
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
