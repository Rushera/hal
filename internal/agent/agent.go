// Package agent implements hal's core agent loop: it turns a user prompt into
// one or more provider requests, executes the tool calls the model asks for,
// and appends every message to the conversation history.
//
// The loop builds a request from system prompt + history + tools, streams the
// assistant response, executes each tool call, records the results, and repeats
// until the model answers without requesting tools.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"
)

// Conversation roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Event types emitted during a run.
const (
	EventTextDelta  = "text_delta"
	EventToolStart  = "tool_start"
	EventToolResult = "tool_result"
	EventTurnEnd    = "turn_end"
	EventUsage      = "usage"
	EventDone       = "done"
	EventError      = "error"
)

// MaxToolResultChars caps a single tool result before it is fed back to the
// model, so one huge command output cannot blow up the context window.
const MaxToolResultChars = 30000

// ToolCall is a single function invocation requested by the model.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message is one entry of the conversation in provider-neutral form.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
	ToolName   string     `json:"toolName,omitempty"`
	// Usage is only set on assistant messages returned by a provider.
	Usage     *Usage `json:"usage,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

// ToolSpec describes a tool to the model.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Usage reports token accounting for one provider request.
type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	TotalTokens  int `json:"totalTokens"`
}

// Tool is implemented by every built-in tool.
type Tool interface {
	Name() string
	Description() string
	// Parameters returns a JSON Schema object for the tool arguments.
	Parameters() json.RawMessage
	// Run executes the tool. A returned error is reported to the model as a
	// failed tool result rather than aborting the run.
	Run(ctx context.Context, args json.RawMessage) (string, error)
}

// Provider streams model responses. The bundled implementation is
// OpenAI-compatible (see internal/provider).
type Provider interface {
	Name() string
	ModelName() string
	Stream(ctx context.Context, messages []Message, tools []ToolSpec, onText func(string)) (Message, error)
}

// ModelSetter is implemented by providers that let the model be switched at
// runtime (used by the TUI's /model command).
type ModelSetter interface{ SetModelName(string) }

// SessionIDSetter is implemented by providers that accept a session affinity
// id, used for routing and prompt-cache affinity on gateways that want one.
type SessionIDSetter interface{ SetSessionID(string) }

// ToolResult is the outcome of one tool execution.
type ToolResult struct {
	Name       string `json:"name"`
	Content    string `json:"content"`
	IsError    bool   `json:"isError"`
	DurationMS int64  `json:"durationMs"`
}

// Event is what the agent reports while running. The TUI and print/JSON modes
// subscribe to these.
type Event struct {
	Type     string      `json:"type"`
	Text     string      `json:"text,omitempty"`
	ToolCall *ToolCall   `json:"toolCall,omitempty"`
	Result   *ToolResult `json:"result,omitempty"`
	Message  *Message    `json:"message,omitempty"`
	Usage    *Usage      `json:"usage,omitempty"`
	Error    string      `json:"error,omitempty"`
}

// Agent drives the loop.
type Agent struct {
	Provider     Provider
	Tools        []Tool
	SystemPrompt string
	History      []Message
	MaxTurns     int
	// OnEvent receives every event; never nil after New.
	OnEvent func(Event)
	// OnMessage receives every message appended to the history, which is how
	// sessions persist themselves.
	OnMessage func(Message)
}

// New creates an agent computing a default system prompt for the working
// directory.
func New(p Provider, t []Tool, cwd string, extraPrompt string) *Agent {
	sys := BuildSystemPrompt(cwd, extraPrompt)
	a := &Agent{Provider: p, Tools: t, SystemPrompt: sys, MaxTurns: 32, OnEvent: func(Event) {}}
	if p != nil {
		a.Provider = p
	}
	return a
}

func nowMilli() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// NewUserMessage builds a user message stamped with the current time.
func NewUserMessage(text string) Message {
	return Message{Role: RoleUser, Content: text, Timestamp: nowMilli()}
}

// ModelName reports the active model.
func (a *Agent) ModelName() string {
	if a.Provider == nil {
		return ""
	}
	return a.Provider.ModelName()
}

// SetModelName switches the model when the provider supports it.
func (a *Agent) SetModelName(name string) bool {
	if s, ok := a.Provider.(ModelSetter); ok && name != "" {
		s.SetModelName(name)
		return true
	}
	return false
}

// SetSessionID forwards the conversation id to the provider when it supports
// one, so the gateway can keep routing and cache affinity stable.
func (a *Agent) SetSessionID(id string) bool {
	if s, ok := a.Provider.(SessionIDSetter); ok && id != "" {
		s.SetSessionID(id)
		return true
	}
	return false
}

// ProviderName reports the active provider.
func (a *Agent) ProviderName() string {
	if a.Provider == nil {
		return ""
	}
	return a.Provider.Name()
}

// Messages returns a copy of the conversation history.
func (a *Agent) Messages() []Message {
	out := make([]Message, len(a.History))
	copy(out, a.History)
	return out
}

// Reset clears the conversation history (TUI /new).
func (a *Agent) Reset() { a.History = nil }

// ToolNames lists the enabled tools in registration order.
func (a *Agent) ToolNames() []string {
	names := make([]string, 0, len(a.Tools))
	for _, t := range a.Tools {
		names = append(names, t.Name())
	}
	return names
}

// RunPrompt runs one user prompt through the agent loop, emitting events as it
// goes. It returns when the model stops requesting tools, or on error.
func (a *Agent) RunPrompt(ctx context.Context, prompt string, emit func(Event)) error {
	if emit == nil {
		emit = a.OnEvent
	}
	if emit == nil {
		emit = func(Event) {}
	}
	if prompt != "" {
		msg := NewUserMessage(prompt)
		a.append(msg, emit)
	}
	if a.Provider == nil {
		err := fmt.Errorf("no model provider configured: set HAL_API_KEY or OPENAI_API_KEY")
		emit(Event{Type: EventError, Error: err.Error()})
		return err
	}

	maxTurns := a.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 32
	}

	for turn := 0; turn < maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		var streamed strings.Builder
		assistant, err := a.Provider.Stream(ctx, a.requestMessages(), a.toolSpecs(), func(delta string) {
			streamed.WriteString(delta)
			emit(Event{Type: EventTextDelta, Text: delta})
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			emit(Event{Type: EventError, Error: err.Error()})
			return err
		}
		if assistant.Content == "" {
			assistant.Content = streamed.String()
		}
		assistant.Role = RoleAssistant
		if assistant.Timestamp == "" {
			assistant.Timestamp = nowMilli()
		}
		if assistant.Usage != nil {
			emit(Event{Type: EventUsage, Usage: assistant.Usage})
		}
		a.append(assistant, emit)

		if len(assistant.ToolCalls) == 0 {
			emit(Event{Type: EventDone})
			return nil
		}

		for i := range assistant.ToolCalls {
			call := assistant.ToolCalls[i]
			emit(Event{Type: EventToolStart, ToolCall: &call})
			result := a.runTool(ctx, call)
			emit(Event{Type: EventToolResult, Result: &result})
			a.append(Message{
				Role:       RoleTool,
				Content:    result.Content,
				ToolCallID: call.ID,
				ToolName:   call.Name,
				Timestamp:  nowMilli(),
			}, emit)
		}
		emit(Event{Type: EventTurnEnd})
	}

	err := fmt.Errorf("agent stopped after %d turns without a final answer", maxTurns)
	emit(Event{Type: EventError, Error: err.Error()})
	return err
}

func (a *Agent) append(m Message, emit func(Event)) {
	a.History = append(a.History, m)
	if a.OnMessage != nil {
		a.OnMessage(m)
	}
	msg := m
	emit(Event{Type: "message", Message: &msg})
}

func (a *Agent) requestMessages() []Message {
	msgs := make([]Message, 0, len(a.History)+1)
	if strings.TrimSpace(a.SystemPrompt) != "" {
		msgs = append(msgs, Message{Role: RoleSystem, Content: a.SystemPrompt})
	}
	msgs = append(msgs, a.History...)
	return msgs
}

func (a *Agent) toolSpecs() []ToolSpec {
	specs := make([]ToolSpec, 0, len(a.Tools))
	for _, t := range a.Tools {
		specs = append(specs, ToolSpec{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return specs
}

func (a *Agent) runTool(ctx context.Context, call ToolCall) ToolResult {
	start := time.Now()
	var tool Tool
	for _, t := range a.Tools {
		if t.Name() == call.Name {
			tool = t
			break
		}
	}
	if tool == nil {
		return ToolResult{
			Name: call.Name,
			Content: fmt.Sprintf("Error: unknown tool %q. Available tools: %s",
				call.Name, strings.Join(a.ToolNames(), ", ")),
			IsError:    true,
			DurationMS: time.Since(start).Milliseconds(),
		}
	}
	args := json.RawMessage(call.Arguments)
	if len(strings.TrimSpace(call.Arguments)) == 0 {
		args = json.RawMessage("{}")
	}
	out, err := tool.Run(ctx, args)
	res := ToolResult{Name: call.Name, DurationMS: time.Since(start).Milliseconds()}
	if err != nil {
		res.IsError = true
		if out != "" {
			res.Content = truncate(out+"\nError: "+err.Error(), MaxToolResultChars)
		} else {
			res.Content = truncate("Error: "+err.Error(), MaxToolResultChars)
		}
		return res
	}
	if strings.TrimSpace(out) == "" {
		out = "(no output)"
	}
	res.Content = truncate(out, MaxToolResultChars)
	return res
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	head := max * 3 / 4
	tail := max - head
	return s[:head] +
		fmt.Sprintf("\n\n... [%d bytes truncated] ...\n\n", len(s)-max) +
		s[len(s)-tail:]
}

// BuildSystemPrompt assembles the default system prompt plus any discovered
// context files / extra instructions.
func BuildSystemPrompt(cwd string, extra string) string {
	var b strings.Builder
	b.WriteString(`You are hal, a minimal coding agent that works in the user's terminal.

You help with programming, research, writing, and file work by inspecting the
working directory, running commands, and editing files. You work autonomously:
you do not ask for permission before each tool call, so be careful and
deliberate about what you change.

Guidelines:
- Answer the user's request directly. Prefer doing the work over describing it.
- Use the read tool before editing a file you have not seen.
- Use the edit tool for targeted changes and write for new files or full rewrites.
- Use the bash tool for builds, tests, git, and anything else the shell does.
- Use grep/find/ls to locate code instead of guessing paths.
- Keep responses concise; report what changed and what you verified.
- If a tool fails, read the error and adjust; do not repeat the same call.
- Never invent command output, file contents, or test results.`)
	b.WriteString("\n\nEnvironment:\n")
	b.WriteString("- Working directory: " + cwd + "\n")
	b.WriteString("- OS/arch: " + runtime.GOOS + "/" + runtime.GOARCH + "\n")
	b.WriteString("- Date: " + time.Now().Format("2006-01-02") + "\n")
	if strings.TrimSpace(extra) != "" {
		b.WriteString("\n" + strings.TrimSpace(extra) + "\n")
	}
	return b.String()
}
