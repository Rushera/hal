package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Rushera/hal/internal/agent"
)

// fakeAgent is a scripted stand-in for *agent.Agent.
type fakeAgent struct {
	mu       sync.Mutex
	model    string
	script   []agent.Event
	blocking bool
	failWith error
	prompts  []string
	aborted  bool
	resets   int
}

func (f *fakeAgent) RunPrompt(ctx context.Context, prompt string, emit func(agent.Event)) error {
	f.mu.Lock()
	f.prompts = append(f.prompts, prompt)
	script := append([]agent.Event(nil), f.script...)
	blocking, failWith := f.blocking, f.failWith
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
func (f *fakeAgent) Reset()                     { f.mu.Lock(); f.resets++; f.mu.Unlock() }
func (f *fakeAgent) ToolNames() []string        { return []string{"read", "bash"} }

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

// sseStream reads server-sent events from a response body into a channel.
type sseStream struct {
	t      *testing.T
	events chan outEvent
	cancel func()
}

func connectSSE(t *testing.T, base, token string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/events?token="+token, nil)
	if err != nil {
		t.Fatalf("building SSE request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("SSE connect: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("SSE connect status = %d", resp.StatusCode)
	}
	s := &sseStream{t: t, events: make(chan outEvent, 128), cancel: cancel}
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev outEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				continue
			}
			select {
			case s.events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return s
}

// waitFor blocks until an event of the given type arrives, or the deadline
// passes. Interior events are discarded.
func (s *sseStream) waitFor(t *testing.T, kind string) outEvent {
	t.Helper()
	deadline := time.After(5 * time.Second)
	var seen []string
	for {
		select {
		case ev := <-s.events:
			if ev.Type == kind {
				return ev
			}
			seen = append(seen, ev.Type)
		case <-deadline:
			t.Fatalf("timed out waiting for %q event (saw %v)", kind, seen)
		}
	}
}

func (s *sseStream) close() { s.cancel() }

func newTestServer(t *testing.T, a Agent, opts ...func(*Options)) *Server {
	t.Helper()
	o := Options{Agent: a, Cwd: "/work/project", Version: "test"}
	for _, fn := range opts {
		fn(&o)
	}
	return New(o)
}

func post(t *testing.T, base, token, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hal-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func TestPromptStreamsEventsAndTranscript(t *testing.T) {
	fake := &fakeAgent{script: []agent.Event{
		{Type: agent.EventTextDelta, Text: "Hello "},
		{Type: agent.EventTextDelta, Text: "world"},
		{Type: agent.EventDone},
	}}
	srv := newTestServer(t, fake)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	snapshot := stream.waitFor(t, "state")
	if len(snapshot.Blocks) != 0 {
		t.Fatalf("fresh console should have an empty transcript, got %+v", snapshot.Blocks)
	}

	resp := post(t, ts.URL, srv.Token(), "/api/prompt", `{"text":"say hi"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("prompt status = %d", resp.StatusCode)
	}

	user := stream.waitFor(t, "user")
	if user.Text != "say hi" {
		t.Fatalf("user event text = %q", user.Text)
	}
	delta := stream.waitFor(t, "delta")
	if delta.Text != "Hello " {
		t.Fatalf("first delta = %q", delta.Text)
	}
	if got := stream.waitFor(t, "delta").Text; got != "world" {
		t.Fatalf("second delta = %q", got)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-stream.events:
			if ev.Type == "status" && !ev.Running {
				goto settled
			}
		case <-deadline:
			t.Fatal("run never settled")
		}
	}
settled:

	snap := srv.snapshotEvent()
	if snap.Running {
		t.Fatal("console should be idle after the run")
	}
	// user block + assistant block
	if len(snap.Blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d: %+v", len(snap.Blocks), snap.Blocks)
	}
	if snap.Blocks[0].Kind != kindUser || snap.Blocks[0].Body != "say hi" {
		t.Fatalf("unexpected user block: %+v", snap.Blocks[0])
	}
	if snap.Blocks[1].Kind != kindAssistant || snap.Blocks[1].Body != "Hello world" {
		t.Fatalf("unexpected assistant block: %+v", snap.Blocks[1])
	}
}

func TestToolEventsRenderIntoTranscript(t *testing.T) {
	call := agent.ToolCall{ID: "1", Name: "read", Arguments: `{"path":"main.go"}`}
	fake := &fakeAgent{script: []agent.Event{
		{Type: agent.EventToolStart, ToolCall: &call},
		{Type: agent.EventToolResult, Result: &agent.ToolResult{Name: "read", Content: "1| package main", DurationMS: 3}},
		{Type: agent.EventTurnEnd},
		{Type: agent.EventTextDelta, Text: "done"},
		{Type: agent.EventDone},
	}}
	srv := newTestServer(t, fake)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	stream.waitFor(t, "state")

	post(t, ts.URL, srv.Token(), "/api/prompt", `{"text":"read it"}`).Body.Close()

	toolEv := stream.waitFor(t, "tool")
	if !strings.Contains(toolEv.Title, "read") || !strings.Contains(toolEv.Title, "main.go") {
		t.Fatalf("tool title = %q", toolEv.Title)
	}
	res := stream.waitFor(t, "tool_result")
	if res.Body != "1| package main" || res.DurationMS != 3 {
		t.Fatalf("tool result = %+v", res)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-stream.events:
			if ev.Type == "status" && !ev.Running {
				goto settled
			}
		case <-deadline:
			t.Fatal("run never settled")
		}
	}
settled:

	snap := srv.snapshotEvent()
	if len(snap.Blocks) != 3 {
		t.Fatalf("expected user+tool+assistant, got %+v", snap.Blocks)
	}
	if b := snap.Blocks[1]; b.Kind != kindTool || b.Body != "1| package main" || b.IsError {
		t.Fatalf("unexpected tool block: %+v", b)
	}
}

func TestToolResultErrorsAreMarked(t *testing.T) {
	call := agent.ToolCall{ID: "1", Name: "bash", Arguments: `{"command":"false"}`}
	fake := &fakeAgent{script: []agent.Event{
		{Type: agent.EventToolStart, ToolCall: &call},
		{Type: agent.EventToolResult, Result: &agent.ToolResult{Name: "bash", Content: "exit code 1", IsError: true}},
		{Type: agent.EventDone},
	}}
	srv := newTestServer(t, fake)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	stream.waitFor(t, "state")

	post(t, ts.URL, srv.Token(), "/api/prompt", `{"text":"run"}`).Body.Close()

	res := stream.waitFor(t, "tool_result")
	if !res.IsError {
		t.Fatal("failed tool result should be flagged")
	}
	waitIdle(t, stream)
	if b := srv.snapshotEvent().Blocks[1]; !b.IsError {
		t.Fatalf("tool block should be marked as an error: %+v", b)
	}
}

func waitIdle(t *testing.T, s *sseStream) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-s.events:
			if ev.Type == "status" && !ev.Running {
				return
			}
		case <-deadline:
			t.Fatal("run never settled")
		}
	}
}

func TestQueuedPromptRunsAfterCurrentTurn(t *testing.T) {
	fake := &fakeAgent{blocking: true}
	srv := newTestServer(t, fake)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	stream.waitFor(t, "state")

	post(t, ts.URL, srv.Token(), "/api/prompt", `{"text":"first"}`).Body.Close()
	stream.waitFor(t, "user")

	// The second prompt must be queued, not rejected or run concurrently.
	resp := post(t, ts.URL, srv.Token(), "/api/prompt", `{"text":"second"}`)
	var queued struct {
		Queued bool `json:"queued"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&queued); err != nil {
		t.Fatalf("decoding queue response: %v", err)
	}
	resp.Body.Close()
	if !queued.Queued {
		t.Fatal("second prompt should have been queued")
	}
	if got := fake.promptCount(); got != 1 {
		t.Fatalf("agent should still be on the first prompt, got %d", got)
	}

	srv.Abort()
	stream.waitFor(t, "system") // "aborted by user"
	// The queued prompt starts as soon as the aborted run returns, which is when
	// the fake records the cancellation.
	stream.waitFor(t, "user")
	if !fake.wasAborted() {
		t.Fatal("abort did not cancel the run")
	}
	if got := fake.promptCount(); got != 2 {
		t.Fatalf("queued prompt should have started, got %d prompts", got)
	}
}

func TestAbortWhenIdleIsANoop(t *testing.T) {
	fake := &fakeAgent{}
	srv := newTestServer(t, fake)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp := post(t, ts.URL, srv.Token(), "/api/abort", `{}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("abort status = %d", resp.StatusCode)
	}
	if fake.wasAborted() {
		t.Fatal("nothing was running, so nothing should have been aborted")
	}
}

func TestAuthRejectsWrongOrMissingToken(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, tc := range []struct{ name, path string }{
		{"index", "/"},
		{"state", "/api/state"},
		{"prompt", "/api/prompt"},
	} {
		resp, err := http.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s without a token = %d, want 403", tc.name, resp.StatusCode)
		}
	}

	resp, err := http.Get(ts.URL + "/?token=wrong")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong token = %d, want 403", resp.StatusCode)
	}
}

func TestIndexServesTheConsoleWithToken(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/?token=" + srv.Token())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<title>hal</title>") {
		t.Fatalf("index is not the console page:\n%s", body)
	}
}

func TestInitialPromptStartsARun(t *testing.T) {
	fake := &fakeAgent{script: []agent.Event{{Type: agent.EventTextDelta, Text: "hi"}, {Type: agent.EventDone}}}
	srv := newTestServer(t, fake, func(o *Options) { o.InitialPrompt = "hello there" })

	deadline := time.After(5 * time.Second)
	for srv.snapshotEvent().Running {
		select {
		case <-deadline:
			t.Fatal("initial prompt never ran")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if got := fake.promptCount(); got != 1 {
		t.Fatalf("initial prompt should have run once, got %d", got)
	}
	blocks := srv.snapshotEvent().Blocks
	if len(blocks) != 2 || blocks[0].Body != "hello there" || blocks[1].Body != "hi" {
		t.Fatalf("unexpected transcript: %+v", blocks)
	}
}

func TestEmptyPromptIsRejected(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp := post(t, ts.URL, srv.Token(), "/api/prompt", `{"text":"   "}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty prompt status = %d, want 400", resp.StatusCode)
	}
}

func TestNewSessionAndModelCommands(t *testing.T) {
	fake := &fakeAgent{}
	srv := newTestServer(t, fake, func(o *Options) {
		o.NewSession = func() (string, error) { return "(session abc)", nil }
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := post(t, ts.URL, srv.Token(), "/api/model", `{"model":"gpt-x"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("model switch status = %d", resp.StatusCode)
	}
	if fake.ModelName() != "gpt-x" {
		t.Fatalf("model not switched: %q", fake.ModelName())
	}

	resp = post(t, ts.URL, srv.Token(), "/api/new", `{}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("new session status = %d", resp.StatusCode)
	}
	if fake.resets != 1 {
		t.Fatalf("new session should reset the agent, resets = %d", fake.resets)
	}
	found := false
	for _, b := range srv.snapshotEvent().Blocks {
		if b.Title == "new session" && strings.Contains(b.Body, "(session abc)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("new session should be noted in the transcript: %+v", srv.snapshotEvent().Blocks)
	}
}

func TestRunFailureIsReportedToThePage(t *testing.T) {
	fake := &fakeAgent{failWith: errors.New("boom")}
	srv := newTestServer(t, fake)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	stream.waitFor(t, "state")

	post(t, ts.URL, srv.Token(), "/api/prompt", `{"text":"go"}`).Body.Close()
	if ev := stream.waitFor(t, "error"); ev.Text != "boom" {
		t.Fatalf("error event = %+v", ev)
	}
	waitIdle(t, stream)
}

func TestURLRewritesWildcardHost(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{})
	for _, tc := range []struct{ addr, want string }{
		{"0.0.0.0:8080", "http://127.0.0.1:8080/"},
		{"[::]:8080", "http://127.0.0.1:8080/"},
		{"127.0.0.1:9000", "http://127.0.0.1:9000/"},
		{":7000", "http://127.0.0.1:7000/"},
	} {
		got := srv.URL(tc.addr)
		if !strings.HasPrefix(got, tc.want) {
			t.Fatalf("URL(%q) = %q, want prefix %q", tc.addr, got, tc.want)
		}
		if !strings.HasSuffix(got, "?token="+srv.Token()) {
			t.Fatalf("URL(%q) = %q, want the access token appended", tc.addr, got)
		}
	}
}
