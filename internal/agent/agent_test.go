package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// scriptedProvider answers with a scripted sequence of failures and replies, so
// the retry policy can be tested without a network.
type scriptedProvider struct {
	calls  int
	script []error  // per-call error, nil to answer
	answer string   // text of a successful answer
	text   []string // deltas emitted before an error, per call
}

func (p *scriptedProvider) Name() string      { return "scripted" }
func (p *scriptedProvider) ModelName() string { return "scripted-model" }

func (p *scriptedProvider) Stream(ctx context.Context, msgs []Message, tools []ToolSpec, onText func(string)) (Message, error) {
	i := p.calls
	p.calls++
	// text[i] is what this call streams before it fails (if it does).
	if i < len(p.text) && p.text[i] != "" {
		onText(p.text[i])
	}
	if i < len(p.script) && p.script[i] != nil {
		return Message{}, p.script[i]
	}
	if p.answer != "" {
		onText(p.answer)
	}
	return Message{Role: RoleAssistant, Content: p.answer}, nil
}

func newRetryAgent(p Provider) (*Agent, *[]Event, *[]time.Duration) {
	var events []Event
	var sleeps []time.Duration
	a := New(p, nil, "/tmp", "")
	a.MaxRetries = 3
	a.RetryBaseDelay = 10 * time.Millisecond
	a.Sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return ctx.Err()
	}
	a.OnEvent = func(ev Event) { events = append(events, ev) }
	return a, &events, &sleeps
}

func TestTransientFailureIsRetriedAndSucceeds(t *testing.T) {
	p := &scriptedProvider{
		script: []error{
			&RetryableError{Err: errors.New("read: connection reset by peer")},
			&RetryableError{Err: errors.New("connection reset by peer")},
			nil,
		},
		answer: "recovered",
	}
	a, events, sleeps := newRetryAgent(p)
	if err := a.RunPrompt(context.Background(), "hi", nil); err != nil {
		t.Fatalf("the run should have recovered: %v", err)
	}
	if p.calls != 3 {
		t.Fatalf("calls = %d, want 3", p.calls)
	}
	if got := len(*sleeps); got != 2 {
		t.Fatalf("backoff sleeps = %d, want 2", got)
	}
	// exponential: 10ms then 20ms
	if (*sleeps)[0] != 10*time.Millisecond || (*sleeps)[1] != 20*time.Millisecond {
		t.Fatalf("backoff not exponential: %v", *sleeps)
	}
	var retries int
	for _, ev := range *events {
		if ev.Type == EventRetry {
			retries++
			if ev.Attempt == 0 || ev.MaxAttempts != 3 || ev.RetryInMS == 0 {
				t.Fatalf("retry event missing details: %+v", ev)
			}
		}
	}
	if retries != 2 {
		t.Fatalf("retry events = %d, want 2", retries)
	}
	if got := a.Messages(); len(got) != 2 || got[1].Content != "recovered" {
		t.Fatalf("the recovered answer should be the only one recorded: %+v", got)
	}
}

func TestRetriesStopAtTheBudget(t *testing.T) {
	boom := func() error { return &RetryableError{Err: errors.New("connection reset by peer")} }
	p := &scriptedProvider{script: []error{boom(), boom(), boom(), boom(), boom()}}
	a, _, sleeps := newRetryAgent(p)
	err := a.RunPrompt(context.Background(), "hi", nil)
	if err == nil {
		t.Fatal("expected the run to fail once the retries are exhausted")
	}
	if p.calls != 4 { // the first try plus 3 retries
		t.Fatalf("calls = %d, want 4", p.calls)
	}
	if len(*sleeps) != 3 {
		t.Fatalf("sleeps = %d, want 3", len(*sleeps))
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("the transport failure should be reported: %v", err)
	}
}

func TestPermanentFailureIsNotRetried(t *testing.T) {
	p := &scriptedProvider{script: []error{errors.New("invalid character 'x' looking for beginning of value")}}
	a, _, sleeps := newRetryAgent(p)
	if err := a.RunPrompt(context.Background(), "hi", nil); err == nil {
		t.Fatal("expected the run to fail")
	}
	if p.calls != 1 {
		t.Fatalf("a permanent failure must not be retried, calls = %d", p.calls)
	}
	if len(*sleeps) != 0 {
		t.Fatalf("no backoff expected, got %v", *sleeps)
	}
}

func TestPartialStreamIsNotRetried(t *testing.T) {
	p := &scriptedProvider{
		script: []error{&RetryableError{Err: errors.New("connection reset by peer")}},
		text:   []string{"half an answer"},
	}
	a, _, sleeps := newRetryAgent(p)
	err := a.RunPrompt(context.Background(), "hi", nil)
	if err == nil {
		t.Fatal("expected the run to fail")
	}
	if p.calls != 1 {
		t.Fatalf("a stream that already delivered text must not be retried, calls = %d", p.calls)
	}
	if len(*sleeps) != 0 {
		t.Fatalf("no backoff expected, got %v", *sleeps)
	}
	if !strings.Contains(err.Error(), "cut off") {
		t.Fatalf("the truncation should be explained: %v", err)
	}
}

func TestRetryAfterFromTheServerWins(t *testing.T) {
	p := &scriptedProvider{script: []error{
		&RetryableError{Err: errors.New("HTTP 429"), RetryAfter: 5 * time.Second},
		nil,
	}}
	a, _, sleeps := newRetryAgent(p)
	if err := a.RunPrompt(context.Background(), "hi", nil); err != nil {
		t.Fatal(err)
	}
	if len(*sleeps) != 1 || (*sleeps)[0] != 5*time.Second {
		t.Fatalf("Retry-After not honored: %v", *sleeps)
	}
}

func TestRetryBackoffIsCapped(t *testing.T) {
	boom := func() error { return &RetryableError{Err: errors.New("reset")} }
	p := &scriptedProvider{script: []error{boom(), boom(), boom(), boom(), boom(), boom()}}
	a, _, sleeps := newRetryAgent(p)
	a.MaxRetries = 6
	a.RetryBaseDelay = time.Second
	_ = a.RunPrompt(context.Background(), "hi", nil)
	for _, d := range *sleeps {
		if d > maxRetryDelay {
			t.Fatalf("backoff %v exceeds the cap %v", d, maxRetryDelay)
		}
	}
	if last := (*sleeps)[len(*sleeps)-1]; last != maxRetryDelay {
		t.Fatalf("backoff should reach the cap, got %v", *sleeps)
	}
}

func TestRetriesCanBeDisabled(t *testing.T) {
	p := &scriptedProvider{script: []error{&RetryableError{Err: errors.New("reset")}}}
	a, _, sleeps := newRetryAgent(p)
	a.MaxRetries = -1
	if err := a.RunPrompt(context.Background(), "hi", nil); err == nil {
		t.Fatal("expected a failure with retries disabled")
	}
	if p.calls != 1 || len(*sleeps) != 0 {
		t.Fatalf("retries should be disabled: calls = %d, sleeps = %v", p.calls, *sleeps)
	}
}

func TestAbortDuringBackoffStopsTheRun(t *testing.T) {
	p := &scriptedProvider{script: []error{&RetryableError{Err: errors.New("reset")}}}
	var events []Event
	a := New(p, nil, "/tmp", "")
	a.MaxRetries = 3
	a.RetryBaseDelay = time.Second
	a.OnEvent = func(ev Event) { events = append(events, ev) }
	ctx, cancel := context.WithCancel(context.Background())
	a.Sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return sleepCtx(ctx, 0)
	}
	err := a.RunPrompt(ctx, "hi", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the abort to stop the run, got %v", err)
	}
	if p.calls != 1 {
		t.Fatalf("an aborted run must not retry, calls = %d", p.calls)
	}
}

func TestSleepCtxHonoursTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled context should stop the wait: %v", err)
	}
	if err := sleepCtx(context.Background(), 0); err != nil {
		t.Fatalf("a zero wait is a no-op: %v", err)
	}
	start := time.Now()
	if err := sleepCtx(context.Background(), 5*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 4*time.Millisecond {
		t.Fatal("sleepCtx returned early")
	}
}

func TestProviderErrorEventIsEmittedOnce(t *testing.T) {
	p := &scriptedProvider{script: []error{errors.New("bad request")}}
	a, events, _ := newRetryAgent(p)
	if err := a.RunPrompt(context.Background(), "hi", nil); err == nil {
		t.Fatal("expected an error")
	}
	var errs int
	for _, ev := range *events {
		if ev.Type == EventError {
			errs++
		}
	}
	if errs != 1 {
		t.Fatalf("error events = %d, want 1", errs)
	}
}
