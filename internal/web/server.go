// Package web serves hal's console as a local web page: the same agent, tools,
// session and event stream the terminal UI draws, rendered in a browser instead
// of a TTY. It is a thin adapter over the agent loop -- every event the TUI would
// render is forwarded to the page over server-sent events.
//
// The page is intentionally plain HTML/CSS/JS with no build step. The server
// binds to a loopback address by default and every request must carry an access
// token, so other pages open in the same browser cannot drive the agent (the
// token defeats cross-site request forgery). The token is reused from a file
// when one is configured, so a restarted process keeps serving the page that is
// already open.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Rushera/hal/internal/agent"
)

//go:embed index.html
var assets embed.FS

// Constants for transcript block kinds, mirroring the terminal UI.
const (
	kindUser      = "user"
	kindAssistant = "assistant"
	kindTool      = "tool"
	kindSystem    = "system"
	kindError     = "error"
)

// defaultAddr keeps the server on loopback: the console can run shell commands,
// so it must not be reachable from the network unless the user asks for it.
const defaultAddr = "127.0.0.1:8080"

// subBuffer is per-client event backlog. A client that cannot keep up is
// disconnected and its EventSource reconnects, resyncing from a full snapshot.
const subBuffer = 4096

// Agent is the slice of *agent.Agent the web console drives. Keeping it an
// interface makes the server unit-testable with a scripted fake.
type Agent interface {
	RunPrompt(ctx context.Context, prompt string, emit func(agent.Event)) error
	ModelName() string
	SetModelName(string) bool
	ProviderName() string
	Messages() []agent.Message
	Reset()
	ToolNames() []string
}

// Options configures the console.
type Options struct {
	Agent   Agent
	Cwd     string
	Version string
	// Context bounds the agent runs; defaults to context.Background().
	Context context.Context
	// InitialPrompt, when set, starts a run as soon as the server is created.
	InitialPrompt string
	// Token is an explicit access token. When it is empty and TokenFile is set,
	// the token stored there is reused, so a restarted process keeps accepting
	// the page that is already open; a random token is generated and persisted
	// when the file does not exist yet.
	Token     string
	TokenFile string
	// SessionInfo describes the current session (/session).
	SessionInfo func() string
	// SessionID reports the id of the current session, if any. It is put in the
	// console URL and in every state snapshot.
	SessionID func() string
	// NewSession starts a fresh session file and returns its description.
	NewSession func() (string, error)
	// OpenSession switches to a stored session by id. The page URL names the
	// session it was opened in, so reloading a tab reopens that conversation.
	OpenSession func(id string) error
}

// Block is one entry of the transcript shown in the page.
type Block struct {
	Kind    string `json:"kind"`
	Title   string `json:"title,omitempty"`
	Body    string `json:"body"`
	IsError bool   `json:"isError,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// outEvent is one message sent to the page. Content events use Text/Body/Title;
// "status" and "state" events describe the console; "state" also carries the
// whole transcript so a reloading client resynchronises.
type outEvent struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	Title      string `json:"title,omitempty"`
	Body       string `json:"body,omitempty"`
	IsError    bool   `json:"isError,omitempty"`
	DurationMS int64  `json:"durationMs,omitempty"`

	// status / state
	Running  bool   `json:"running"`
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	Status   string `json:"status,omitempty"`
	UsageIn  int    `json:"usageIn"`
	UsageOut int    `json:"usageOut"`
	Turns    int    `json:"turns"`
	Session  string `json:"session,omitempty"`
	// SessionID is the current session's id, which the page keeps in its URL.
	SessionID string   `json:"sessionId,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`
	Version   string   `json:"version,omitempty"`
	Tools     []string `json:"tools,omitempty"`
	Blocks    []Block  `json:"blocks,omitempty"`
}

// Server holds the console state shared by every connected page.
type Server struct {
	opts    Options
	baseCtx context.Context
	token   string

	mu       sync.Mutex
	subs     map[chan []byte]struct{}
	blocks   []Block
	cur      int // index of the streaming assistant block, -1 if none
	curTool  int // index of the running tool block, -1 if none
	running  bool
	aborting bool
	cancel   context.CancelFunc
	queued   []string
	status   string
	usageIn  int
	usageOut int
	turns    int
	lastErr  error
}

// New builds the console, resolving its access token, restoring any stored
// conversation into the transcript, and starting the initial prompt if one was
// supplied.
func New(opts Options) *Server {
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	token := strings.TrimSpace(opts.Token)
	if token == "" && opts.TokenFile != "" {
		t, err := loadOrCreateToken(opts.TokenFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hal web: cannot use the token file %s: %v\n", opts.TokenFile, err)
		} else {
			token = t
		}
	}
	if token == "" {
		token = newToken()
	}
	s := &Server{
		opts:    opts,
		baseCtx: opts.Context,
		token:   token,
		subs:    map[chan []byte]struct{}{},
		cur:     -1,
		curTool: -1,
		status:  "ready",
	}
	s.seedFromHistory()
	if strings.TrimSpace(opts.InitialPrompt) != "" {
		_, _ = s.Submit(opts.InitialPrompt)
	}
	return s
}

// Token is the access token required by every request.
func (s *Server) Token() string { return s.token }

// loadOrCreateToken returns the token persisted at path, writing a fresh one
// with owner-only permissions when the file is missing or empty.
func loadOrCreateToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if tok := strings.TrimSpace(string(data)); tok != "" {
			return tok, nil
		}
	case !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	tok := newToken()
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

func newToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// SessionID is the id of the session the console is serving, or "" when
// sessions are disabled.
func (s *Server) SessionID() string {
	if s.opts.SessionID == nil {
		return ""
	}
	return strings.TrimSpace(s.opts.SessionID())
}

// URL renders the address the page should be opened at, rewriting wildcard
// hosts, and including the access token and the current session.
func (s *Server) URL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err == nil {
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		addr = net.JoinHostPort(host, port)
	}
	u := fmt.Sprintf("http://%s/?token=%s", addr, s.token)
	if id := s.SessionID(); id != "" {
		u += "&session=" + url.QueryEscape(id)
	}
	return u
}

// ListenAndServe serves the console until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	if strings.TrimSpace(addr) == "" {
		addr = defaultAddr
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	fmt.Fprintf(os.Stderr, "hal web console: %s\n", s.URL(ln.Addr().String()))

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		s.Abort()
		return srv.Shutdown(shutCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Handler returns the HTTP routes. Exposed so tests can drive the console with
// httptest instead of a real socket.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.auth(s.handleIndex))
	mux.HandleFunc("/api/state", s.auth(s.handleState))
	mux.HandleFunc("/api/events", s.auth(s.handleEvents))
	mux.HandleFunc("/api/prompt", s.auth(s.handlePrompt))
	mux.HandleFunc("/api/abort", s.auth(s.handleAbort))
	mux.HandleFunc("/api/clear", s.auth(s.handleClear))
	mux.HandleFunc("/api/new", s.auth(s.handleNew))
	mux.HandleFunc("/api/model", s.auth(s.handleModel))
	return mux
}

// auth requires the startup token, so a page on another origin cannot reach the
// API even though it can reach loopback.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Hal-Token")
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) != 1 {
			http.Error(w, "invalid or missing token", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	// The URL names the session this tab was opened in; reopen it before the
	// page subscribes, so its first snapshot already holds the conversation.
	s.adoptSession(r.URL.Query().Get("session"))
	page, err := assets.ReadFile("index.html")
	if err != nil {
		http.Error(w, "console assets are missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}

// adoptSession switches the console to the session named in the page URL. It is
// a no-op when that session is already current, when no opener is configured, or
// while a turn is running.
func (s *Server) adoptSession(id string) {
	id = strings.TrimSpace(id)
	if id == "" || s.opts.OpenSession == nil || id == s.SessionID() {
		return
	}
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running {
		return
	}
	if err := s.opts.OpenSession(id); err != nil {
		fmt.Fprintf(os.Stderr, "hal web: cannot open session %s: %v\n", id, err)
		return
	}
	s.seedFromHistory()
	s.mu.Lock()
	s.broadcastLocked(s.statusLocked("state"))
	s.mu.Unlock()
}

// seedFromHistory replaces the transcript with one rebuilt from the conversation
// the agent holds, so a page that loads after a restart shows the whole session
// instead of an empty console.
func (s *Server) seedFromHistory() {
	blocks := historyBlocks(s.opts.Agent)
	if len(blocks) > 0 {
		blocks = append(blocks, Block{
			Kind:  kindSystem,
			Title: "restored",
			Body:  fmt.Sprintf("%d messages from the stored session", len(blocks)),
			Done:  true,
		})
	}
	s.mu.Lock()
	s.blocks = blocks
	s.cur, s.curTool = -1, -1
	s.mu.Unlock()
}

// historyBlocks renders stored conversation messages as console blocks, reusing
// the same shapes the live event stream produces: one block per user turn and
// assistant answer, one per tool call with its result folded in.
func historyBlocks(a Agent) []Block {
	if a == nil {
		return nil
	}
	var blocks []Block
	toolAt := map[string]int{}
	for _, m := range a.Messages() {
		switch m.Role {
		case agent.RoleUser:
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			blocks = append(blocks, Block{Kind: kindUser, Body: m.Content, Done: true})
		case agent.RoleAssistant:
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, Block{Kind: kindAssistant, Body: m.Content, Done: true})
			}
			for _, call := range m.ToolCalls {
				title := call.Name + "(" + summarizeArgs(call.Arguments) + ")"
				if call.ID != "" {
					toolAt[call.ID] = len(blocks)
				}
				blocks = append(blocks, Block{Kind: kindTool, Title: title})
			}
		case agent.RoleTool:
			at, ok := toolAt[m.ToolCallID]
			if !ok || at >= len(blocks) {
				continue
			}
			blocks[at].Body = m.Content
			blocks[at].IsError = toolResultFailed(m.Content)
			blocks[at].Done = true
		}
	}
	return blocks
}

// toolResultFailed reports whether a stored tool result looks like a failure.
// The result's IsError flag is not part of the session file, but a failed tool
// appends its error as "Error: ...".
func toolResultFailed(content string) bool {
	return strings.HasPrefix(content, "Error: ") || strings.Contains(content, "\nError: ")
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.snapshotEvent())
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	queued, err := s.Submit(body.Text)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "queued": queued})
}

func (s *Server) handleAbort(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	s.Abort()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	s.ClearTranscript()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleNew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if err := s.NewSession(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !s.SetModel(strings.TrimSpace(body.Model)) {
		writeError(w, http.StatusConflict, "this provider cannot switch models at runtime")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported by this connection", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ch := make(chan []byte, subBuffer)
	s.addSub(ch)
	defer s.removeSub(ch)

	// Send the current state first so a fresh (or reconnecting) page is complete
	// before it applies any delta.
	if data, err := json.Marshal(s.snapshotEvent()); err == nil {
		writeSSE(w, data)
		flusher.Flush()
	}

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case data, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, data)
			flusher.Flush()
		case <-ticker.C:
			_, _ = io.WriteString(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func writeSSE(w io.Writer, data []byte) {
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n\n"))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// ---- subscriptions ---------------------------------------------------------

func (s *Server) addSub(ch chan []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs[ch] = struct{}{}
}

func (s *Server) removeSub(ch chan []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[ch]; ok {
		delete(s.subs, ch)
		close(ch)
	}
}

// broadcastLocked queues an event for every subscriber. A client whose backlog is
// full is dropped; its EventSource reconnects and resyncs from a snapshot.
func (s *Server) broadcastLocked(ev outEvent) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	for ch := range s.subs {
		select {
		case ch <- data:
		default:
			delete(s.subs, ch)
			close(ch)
		}
	}
}

// emit broadcasts an event that does not change server state.
func (s *Server) emit(ev outEvent) {
	s.mu.Lock()
	s.broadcastLocked(ev)
	s.mu.Unlock()
}

// statusLocked builds a status/snapshot event; callers hold s.mu.
func (s *Server) statusLocked(evType string) outEvent {
	ev := outEvent{
		Type:     evType,
		Running:  s.running,
		Status:   s.status,
		UsageIn:  s.usageIn,
		UsageOut: s.usageOut,
		Turns:    s.turns,
		Cwd:      s.opts.Cwd,
		Version:  s.opts.Version,
	}
	if s.opts.Agent != nil {
		ev.Model = s.opts.Agent.ModelName()
		ev.Provider = s.opts.Agent.ProviderName()
		ev.Tools = s.opts.Agent.ToolNames()
	}
	if s.opts.SessionInfo != nil {
		ev.Session = s.opts.SessionInfo()
	}
	ev.SessionID = s.SessionID()
	if evType == "state" {
		ev.Blocks = append([]Block(nil), s.blocks...)
	}
	return ev
}

func (s *Server) snapshotEvent() outEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked("state")
}

// ---- console actions -------------------------------------------------------

// ErrEmptyPrompt is returned when a blank prompt is submitted.
var ErrEmptyPrompt = errors.New("prompt is empty")

// Submit starts a run, or queues the prompt when one is already running. It
// reports whether the prompt was queued.
func (s *Server) Submit(text string) (bool, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return false, ErrEmptyPrompt
	}
	s.mu.Lock()
	if s.running {
		s.queued = append(s.queued, text)
		body := "queued (runs after the current turn): " + firstLine(text)
		s.blocks = append(s.blocks, Block{Kind: kindSystem, Title: "queued", Body: body, Done: true})
		s.broadcastLocked(outEvent{Type: "system", Title: "queued", Text: body})
		s.mu.Unlock()
		return true, nil
	}
	ctx, cancel := s.claimRunLocked()
	s.mu.Unlock()
	go s.run(ctx, cancel, text)
	return false, nil
}

// claimRunLocked marks the console busy and returns a cancellable context for the
// new run; callers hold s.mu.
func (s *Server) claimRunLocked() (context.Context, context.CancelFunc) {
	s.running = true
	s.status = "working"
	s.aborting = false
	s.lastErr = nil
	s.cur, s.curTool = -1, -1
	ctx, cancel := context.WithCancel(s.baseCtx)
	s.cancel = cancel
	s.broadcastLocked(s.statusLocked("status"))
	return ctx, cancel
}

func (s *Server) run(ctx context.Context, cancel context.CancelFunc, prompt string) {
	s.mu.Lock()
	s.blocks = append(s.blocks, Block{Kind: kindUser, Body: prompt, Done: true})
	s.broadcastLocked(outEvent{Type: "user", Text: prompt})
	s.mu.Unlock()

	ag := s.opts.Agent
	var err error
	if ag == nil {
		err = errors.New("no agent configured")
	} else {
		err = ag.RunPrompt(ctx, prompt, s.applyEvent)
	}
	cancel()
	s.finishRun(err)
}

// applyEvent folds one agent event into the transcript and forwards the
// corresponding delta to the page. It runs on the run goroutine.
func (s *Server) applyEvent(ev agent.Event) {
	s.mu.Lock()
	var out outEvent
	switch ev.Type {
	case agent.EventTextDelta:
		if s.cur < 0 {
			s.blocks = append(s.blocks, Block{Kind: kindAssistant})
			s.cur = len(s.blocks) - 1
		}
		s.blocks[s.cur].Body += ev.Text
		out = outEvent{Type: "delta", Text: ev.Text}
	case agent.EventToolStart:
		s.cur = -1
		if ev.ToolCall != nil {
			title := ev.ToolCall.Name + "(" + summarizeArgs(ev.ToolCall.Arguments) + ")"
			s.blocks = append(s.blocks, Block{Kind: kindTool, Title: title})
			s.curTool = len(s.blocks) - 1
			out = outEvent{Type: "tool", Title: title}
		}
	case agent.EventToolResult:
		if ev.Result != nil && s.curTool >= 0 && s.curTool < len(s.blocks) {
			b := &s.blocks[s.curTool]
			b.Body = ev.Result.Content
			b.IsError = ev.Result.IsError
			b.Done = true
			out = outEvent{Type: "tool_result", Body: b.Body, IsError: b.IsError,
				DurationMS: ev.Result.DurationMS, Title: b.Title}
		}
		s.cur, s.curTool = -1, -1
	case agent.EventTurnEnd:
		s.cur = -1
		s.turns++
		out = s.statusLocked("status")
	case agent.EventUsage:
		if ev.Usage != nil {
			s.usageIn += ev.Usage.InputTokens
			s.usageOut += ev.Usage.OutputTokens
		}
		out = s.statusLocked("status")
	case agent.EventError:
		s.lastErr = errors.New(ev.Error)
		s.blocks = append(s.blocks, Block{Kind: kindError, Title: "error", Body: ev.Error, IsError: true, Done: true})
		out = outEvent{Type: "error", Title: "error", Text: ev.Error}
	}
	if out.Type != "" {
		s.broadcastLocked(out)
	}
	s.mu.Unlock()
}

// finishRun clears the busy state, reports a failure, and starts the next queued
// prompt if there is one.
func (s *Server) finishRun(err error) {
	s.mu.Lock()
	aborted := s.aborting
	s.aborting = false
	s.running = false
	s.cur, s.curTool = -1, -1
	s.cancel = nil
	if err != nil && !aborted && !errors.Is(err, context.Canceled) && s.lastErr == nil {
		s.lastErr = err
		s.blocks = append(s.blocks, Block{Kind: kindError, Title: "run failed", Body: err.Error(), IsError: true, Done: true})
		s.broadcastLocked(outEvent{Type: "error", Title: "run failed", Text: err.Error()})
	}
	if len(s.queued) > 0 {
		next := s.queued[0]
		s.queued = s.queued[1:]
		ctx, cancel := s.claimRunLocked()
		go s.run(ctx, cancel, next)
	} else {
		s.status = "ready"
	}
	s.broadcastLocked(s.statusLocked("status"))
	s.mu.Unlock()
}

// Abort cancels the running turn, if any.
func (s *Server) Abort() {
	s.mu.Lock()
	if !s.running || s.cancel == nil {
		s.mu.Unlock()
		return
	}
	s.aborting = true
	s.status = "aborting"
	cancel := s.cancel
	s.blocks = append(s.blocks, Block{Kind: kindSystem, Body: "aborted by user", Done: true})
	s.broadcastLocked(outEvent{Type: "system", Text: "aborted by user"})
	s.broadcastLocked(s.statusLocked("status"))
	s.mu.Unlock()
	cancel()
}

// ClearTranscript empties the visible transcript without touching the
// conversation history.
func (s *Server) ClearTranscript() {
	s.mu.Lock()
	s.blocks = nil
	s.cur, s.curTool = -1, -1
	s.broadcastLocked(outEvent{Type: "clear"})
	s.mu.Unlock()
}

// NewSession clears the conversation and, when sessions are enabled, starts a
// fresh session file.
func (s *Server) NewSession() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return errors.New("cannot start a new session while a turn is running")
	}
	if s.opts.Agent == nil {
		return errors.New("no agent configured")
	}
	s.opts.Agent.Reset()
	s.blocks = nil
	s.cur, s.curTool = -1, -1
	s.turns, s.usageIn, s.usageOut = 0, 0, 0
	s.lastErr = nil
	desc := ""
	if s.opts.NewSession != nil {
		d, err := s.opts.NewSession()
		if err != nil {
			return err
		}
		desc = " " + d
	}
	s.blocks = append(s.blocks, Block{Kind: kindSystem, Title: "new session", Body: "conversation cleared" + desc, Done: true})
	s.broadcastLocked(outEvent{Type: "clear"})
	s.broadcastLocked(s.statusLocked("state"))
	return nil
}

// SetModel switches the model when the provider supports it.
func (s *Server) SetModel(name string) bool {
	if s.opts.Agent == nil || name == "" {
		return false
	}
	if !s.opts.Agent.SetModelName(name) {
		return false
	}
	s.mu.Lock()
	s.blocks = append(s.blocks, Block{Kind: kindSystem, Title: "model", Body: "switched to " + name, Done: true})
	s.broadcastLocked(outEvent{Type: "system", Title: "model", Text: "switched to " + name})
	s.broadcastLocked(s.statusLocked("status"))
	s.mu.Unlock()
	return true
}

// ---- small helpers ---------------------------------------------------------

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
