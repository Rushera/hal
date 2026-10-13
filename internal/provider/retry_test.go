package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rushera/hal/internal/agent"
)

// transient checks whether err asks the agent to retry.
func transient(err error) bool {
	var re *agent.RetryableError
	return errors.As(err, &re)
}

func TestConnectionResetIsTransient(t *testing.T) {
	cases := map[string]bool{
		`Post "https://x/v1/chat/completions": read tcp 1.2.3.4:5->6.7.8.9:443: read: connection reset by peer`: true,
		`read: connection reset by peer`:                       true,
		`dial tcp: connection refused`:                         true,
		`unexpected EOF`:                                       true,
		`write tcp: broken pipe`:                               true,
		`i/o timeout`:                                          true,
		`http2: server sent GOAWAY`:                            true,
		`context deadline exceeded`:                            false,
		`invalid character 'x' looking for beginning of value`: false,
		`x509: certificate signed by unknown authority`:        false,
	}
	for msg, want := range cases {
		if got := isTransientNetError(errors.New(msg)); got != want {
			t.Fatalf("isTransientNetError(%q) = %v, want %v", msg, got, want)
		}
	}
	if isTransientNetError(nil) {
		t.Fatal("nil is not a transient error")
	}
	var ne net.Error = &net.DNSError{Err: "boom", IsTimeout: true}
	if !isTransientNetError(ne) {
		t.Fatal("a timeout must be retried")
	}
	if !isTransientNetError(fmt.Errorf("wrapped: %w", io.ErrUnexpectedEOF)) {
		t.Fatal("a wrapped unexpected EOF must be retried")
	}
}

func TestTransientHTTPStatuses(t *testing.T) {
	for code, want := range map[int]bool{
		400: false, 401: false, 403: false, 404: false, 422: false,
		408: true, 429: true, 500: true, 502: true, 503: true, 504: true, 529: true,
	} {
		if got := isTransientStatus(code); got != want {
			t.Fatalf("isTransientStatus(%d) = %v, want %v", code, got, want)
		}
	}
}

func TestRetryAfterParsing(t *testing.T) {
	if got := retryAfter("3"); got != 3*time.Second {
		t.Fatalf("seconds form = %v", got)
	}
	if got := retryAfter(""); got != 0 {
		t.Fatalf("empty = %v", got)
	}
	if got := retryAfter("not-a-date"); got != 0 {
		t.Fatalf("garbage = %v", got)
	}
	when := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	if got := retryAfter(when); got <= 0 || got > 2*time.Second {
		t.Fatalf("date form = %v", got)
	}
}

func TestRateLimitCarriesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		http.Error(w, `{"error":{"message":"slow down"}}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	o := NewFromEnv("k", srv.URL, "m")
	_, err := o.Stream(context.Background(), []agent.Message{agent.NewUserMessage("hi")}, nil, nil)
	var re *agent.RetryableError
	if !errors.As(err, &re) {
		t.Fatalf("429 should be retryable: %v", err)
	}
	if re.RetryAfter != 2*time.Second {
		t.Fatalf("Retry-After not honored: %v", re.RetryAfter)
	}
	if !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("the message should keep the status and body: %v", err)
	}
}

func TestServerErrorIsRetryableButBadRequestIsNot(t *testing.T) {
	code := http.StatusInternalServerError
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", code)
	}))
	defer srv.Close()
	o := NewFromEnv("k", srv.URL, "m")

	_, err := o.Stream(context.Background(), []agent.Message{agent.NewUserMessage("hi")}, nil, nil)
	if !transient(err) {
		t.Fatalf("500 should be retryable: %v", err)
	}

	code = http.StatusBadRequest
	_, err = o.Stream(context.Background(), []agent.Message{agent.NewUserMessage("hi")}, nil, nil)
	if transient(err) {
		t.Fatalf("400 must not be retried: %v", err)
	}
}

func TestStreamFailureMidwayIsRetryable(t *testing.T) {
	// The server sends a valid chunk, then drops the connection without the
	// terminating [DONE], which is what a reset mid-stream looks like.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close() // reset without a clean end
	}))
	defer srv.Close()

	o := NewFromEnv("k", srv.URL, "m")
	msg, err := o.Stream(context.Background(), []agent.Message{agent.NewUserMessage("hi")}, nil, nil)
	if err == nil {
		t.Fatalf("a truncated stream must be an error, got %q", msg.Content)
	}
	if !transient(err) {
		t.Fatalf("a truncated stream should be retryable: %v", err)
	}
}

func TestAbortIsNotRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	o := NewFromEnv("k", srv.URL, "m")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := o.Stream(ctx, []agent.Message{agent.NewUserMessage("hi")}, nil, nil)
	if transient(err) {
		t.Fatalf("an aborted request must not be retried: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestClientReusesConnections(t *testing.T) {
	var conns int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateNew {
			atomic.AddInt32(&conns, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	o := NewFromEnv("k", srv.URL, "m")
	for i := 0; i < 3; i++ {
		if _, err := o.Stream(context.Background(), []agent.Message{agent.NewUserMessage("hi")}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&conns); got != 1 {
		t.Fatalf("expected one reused connection, got %d", got)
	}
}

func TestEmptyStreamIsRetryable(t *testing.T) {
	// Headers, then a close with no chunks at all: a reset that would otherwise
	// look like an empty answer.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	o := NewFromEnv("k", srv.URL, "m")
	_, err := o.Stream(context.Background(), []agent.Message{agent.NewUserMessage("hi")}, nil, nil)
	if !transient(err) {
		t.Fatalf("an empty stream should be retryable: %v", err)
	}
}

func TestCompleteStreamIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"all done\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	o := NewFromEnv("k", srv.URL, "m")
	msg, err := o.Stream(context.Background(), []agent.Message{agent.NewUserMessage("hi")}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "all done" {
		t.Fatalf("content = %q", msg.Content)
	}
}
