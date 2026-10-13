// Package provider implements hal's model access layer: talking to a model.
// The bundled client speaks the OpenAI chat-completions protocol, which also
// covers OpenAI-compatible servers (OpenRouter, Groq, Ollama, llama.cpp,
// vLLM, LM Studio, ...).
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Rushera/hal/internal/agent"
)

// DefaultBaseURL is used when no endpoint is configured.
const DefaultBaseURL = "https://api.openai.com/v1"

// DefaultModel is used when no model is configured.
const DefaultModel = "gpt-4o-mini"

// Transient failures: the agent retries these with backoff.
const (
	// HeaderTimeout bounds the wait for the response headers (and for a
	// connection). It deliberately does not cover the streamed body: a long
	// answer must not be cut off, only a server that never answers.
	HeaderTimeout = 60 * time.Second
	// maxErrorBody caps how much of a failed response is read for the message.
	maxErrorBody = 8192
	// maxIdleConnsPerHost keeps a gateway's connections warm, which matters for
	// the retries above: a fresh connection to a flaky endpoint may reset again.
	maxIdleConnsPerHost = 8
)

// retryAfterHeader is the standard header naming how long to wait.
const retryAfterHeader = "Retry-After"

// OpenAI is an OpenAI-compatible chat completions client.
type OpenAI struct {
	APIKey      string
	BaseURL     string
	Model       string
	HTTPClient  *http.Client
	MaxTokens   int
	Temperature *float64

	// UserAgent identifies this client. Gateways that route coding-agent
	// traffic ask clients to name themselves instead of using the default
	// Go-http-client user agent.
	UserAgent string
	// SessionID is the stable per-conversation id. Several gateways use it for
	// routing and prompt-cache affinity.
	SessionID string
	// SessionHeader names the header carrying SessionID. Empty disables it.
	SessionHeader string
}

// NewFromEnv builds a provider from environment variables/flags. Precedence:
// explicit arguments, then HAL_* then OPENAI_* then defaults.
func NewFromEnv(apiKey, baseURL, model string) *OpenAI {
	if apiKey == "" {
		apiKey = firstEnv("HAL_API_KEY", "OPENAI_API_KEY")
	}
	if baseURL == "" {
		baseURL = firstEnv("HAL_BASE_URL", "OPENAI_BASE_URL", "OPENAI_API_BASE")
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if model == "" {
		model = firstEnv("HAL_MODEL", "OPENAI_MODEL")
	}
	if model == "" {
		model = DefaultModel
	}
	return &OpenAI{
		APIKey:     apiKey,
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Model:      model,
		HTTPClient: newHTTPClient(HeaderTimeout),
	}
}

// newHTTPClient builds the client used for streaming requests. The timeout only
// bounds the wait for response headers, and the transport is tuned for the
// connection resets common to flaky gateways.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   maxIdleConnsPerHost,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// Name() etc.
func (o *OpenAI) Name() string      { return "openai-compatible" }
func (o *OpenAI) ModelName() string { return o.Model }

// SetModelName switches the model at runtime (TUI /model).
func (o *OpenAI) SetModelName(m string) { o.Model = m }

// SetSessionID updates the session affinity id sent to the provider.
func (o *OpenAI) SetSessionID(id string) { o.SessionID = id }

func (o *OpenAI) client() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return http.DefaultClient
}

// ---- wire types -----------------------------------------------------------

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Tools       []wireTool    `json:"tools,omitempty"`
	Stream      bool          `json:"stream"`
	StreamUsage bool          `json:"stream_options,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Stream sends the conversation and streams the assistant response, calling
// onText for every text delta. The returned message carries the full text and
// any tool calls the model requested.
func (o *OpenAI) Stream(ctx context.Context, messages []agent.Message, tools []agent.ToolSpec, onText func(string)) (agent.Message, error) {
	if o.Model == "" {
		return agent.Message{}, fmt.Errorf("no model selected (use --model, /model, or HAL_MODEL)")
	}

	req := chatRequest{
		Model:       o.Model,
		Messages:    convertMessages(messages),
		Stream:      true,
		StreamUsage: true,
		MaxTokens:   o.MaxTokens,
		Temperature: o.Temperature,
	}
	for _, t := range tools {
		var wt wireTool
		wt.Type = "function"
		wt.Function.Name = t.Name
		wt.Function.Description = t.Description
		wt.Function.Parameters = t.Parameters
		if len(wt.Function.Parameters) == 0 {
			wt.Function.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		req.Tools = append(req.Tools, wt)
	}

	body, err := json.Marshal(req)
	if err != nil {
		return agent.Message{}, err
	}
	url := o.BaseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return agent.Message{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if o.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	if o.UserAgent != "" {
		httpReq.Header.Set("User-Agent", o.UserAgent)
	}
	if o.SessionHeader != "" && o.SessionID != "" {
		httpReq.Header.Set(o.SessionHeader, o.SessionID)
	}

	resp, err := o.client().Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			// The user aborted; that is not a transport failure to retry.
			return agent.Message{}, ctx.Err()
		}
		return agent.Message{}, classifyRequestError(url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		// Drain the rest so the connection can be reused for the retry.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		statusErr := fmt.Errorf("%s returned HTTP %d: %s", o.Name(), resp.StatusCode, strings.TrimSpace(string(raw)))
		if isTransientStatus(resp.StatusCode) {
			return agent.Message{}, &agent.RetryableError{
				Err:        statusErr,
				RetryAfter: retryAfter(resp.Header.Get(retryAfterHeader)),
			}
		}
		return agent.Message{}, statusErr
	}

	msg, err := o.readStream(resp.Body, onText)
	if err != nil {
		if ctx.Err() != nil {
			return agent.Message{}, ctx.Err()
		}
		// A stream that dies half-way is the reset the user sees: it is worth
		// retrying when no text has been delivered yet (the agent checks that).
		if isTransientNetError(err) {
			return agent.Message{}, &agent.RetryableError{Err: err}
		}
	}
	return msg, err
}

// classifyRequestError wraps transport failures that are worth retrying. A
// connection reset, a dropped connection and a timeout are all transient; a
// malformed URL or a TLS failure is not.
func classifyRequestError(url string, err error) error {
	wrapped := fmt.Errorf("request to %s failed: %w", url, err)
	if isTransientNetError(err) {
		return &agent.RetryableError{Err: wrapped}
	}
	return wrapped
}

// isTransientNetError reports whether a transport-level failure is worth
// retrying: connection resets, dropped connections, broken pipes and timeouts.
func isTransientNetError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true // timeouts and other temporary network errors
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return true
	}
	// The reset that motivated this shows up as a plain error string from the
	// TLS/HTTP stack ("read: connection reset by peer"), so match on text too.
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"connection reset",
		"connection refused",
		"broken pipe",
		"unexpected eof",
		"eof",
		"reset by peer",
		"server closed idle connection",
		"http2: server sent goaway",
		"stream error",
		"i/o timeout",
		"temporarily unavailable",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// isTransientStatus reports whether an HTTP status is worth retrying: rate
// limits and server-side failures.
func isTransientStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return code >= 500 && code <= 599
}

// retryAfter parses the Retry-After header, in seconds or as an HTTP date.
func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func (o *OpenAI) readStream(r io.Reader, onText func(string)) (agent.Message, error) {
	type acc struct {
		id   string
		name string
		args strings.Builder
	}
	var (
		out      agent.Message
		text     strings.Builder
		toolAccs = map[int]*acc{}
		order    []int
		usage    *agent.Usage
		sawDone  bool
	)

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // ignore keep-alives / non-JSON noise
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			return agent.Message{}, fmt.Errorf("provider error: %s", chunk.Error.Message)
		}
		if chunk.Usage != nil {
			usage = &agent.Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
				TotalTokens:  chunk.Usage.TotalTokens,
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if delta.Content != "" {
			text.WriteString(delta.Content)
			if onText != nil {
				onText(delta.Content)
			}
		}
		for _, tc := range delta.ToolCalls {
			a := toolAccs[tc.Index]
			if a == nil {
				a = &acc{}
				toolAccs[tc.Index] = a
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				a.id = tc.ID
			}
			if tc.Function.Name != "" {
				a.name = tc.Function.Name
			}
			a.args.WriteString(tc.Function.Arguments)
		}
	}
	if err := scanner.Err(); err != nil {
		return agent.Message{}, fmt.Errorf("stream read error: %w", err)
	}
	if !sawDone && text.Len() == 0 && len(order) == 0 {
		// The server ended the response without a single chunk and without the
		// [DONE] marker: a reset right after the headers, which is worth
		// retrying rather than presenting as an empty answer.
		return agent.Message{}, &agent.RetryableError{
			Err: fmt.Errorf("the stream ended before the model sent anything"),
		}
	}

	out.Content = text.String()
	out.Role = agent.RoleAssistant
	out.Usage = usage
	for _, idx := range order {
		a := toolAccs[idx]
		if a.name == "" {
			continue
		}
		id := a.id
		if id == "" {
			id = fmt.Sprintf("call_%d", idx)
		}
		args := a.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out.ToolCalls = append(out.ToolCalls, agent.ToolCall{ID: id, Name: a.name, Arguments: args})
	}
	return out, nil
}

func convertMessages(messages []agent.Message) []wireMessage {
	out := make([]wireMessage, 0, len(messages))
	for _, m := range messages {
		content := m.Content
		switch m.Role {
		case agent.RoleTool:
			if strings.TrimSpace(content) == "" {
				content = "(no output)"
			}
		case agent.RoleAssistant:
			// content may legitimately be empty when only tool calls were made
		default:
			if strings.TrimSpace(content) == "" {
				continue
			}
		}
		wm := wireMessage{Role: m.Role, Content: content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			var wtc wireToolCall
			wtc.ID = tc.ID
			wtc.Type = "function"
			wtc.Function.Name = tc.Name
			wtc.Function.Arguments = tc.Arguments
			if wtc.ID == "" {
				wtc.ID = "call_" + tc.Name
			}
			if strings.TrimSpace(wtc.Function.Arguments) == "" {
				wtc.Function.Arguments = "{}"
			}
			wm.ToolCalls = append(wm.ToolCalls, wtc)
		}
		if m.Role == agent.RoleAssistant && wm.Content == "" && len(wm.ToolCalls) == 0 {
			continue
		}
		out = append(out, wm)
	}
	return out
}

// Ping performs a cheap credential/model check used at startup in print mode.
func (o *OpenAI) Ping(ctx context.Context) error {
	if o.APIKey == "" {
		return fmt.Errorf("no API key found: set HAL_API_KEY or OPENAI_API_KEY, or pass --api-key")
	}
	return nil
}

// WithTimeout returns a shallow copy using the given HTTP timeout.
func (o *OpenAI) WithTimeout(d time.Duration) *OpenAI {
	cp := *o
	cp.HTTPClient = &http.Client{Timeout: d}
	return &cp
}
