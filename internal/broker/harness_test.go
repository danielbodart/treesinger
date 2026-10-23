package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// silentLogger discards output; keeps test runs quiet while still satisfying
// Logger.
type silentLogger struct{}

func (silentLogger) Log(...any)   {}
func (silentLogger) Warn(...any)  {}
func (silentLogger) Error(...any) {}

// recordingLogger keeps every line, for the tests that assert on logging.
type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordingLogger) Log(args ...any)   { l.record("log", args) }
func (l *recordingLogger) Warn(args ...any)  { l.record("warn", args) }
func (l *recordingLogger) Error(args ...any) { l.record("error", args) }

func (l *recordingLogger) record(level string, args []any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+": "+strings.TrimSuffix(fmt.Sprintln(args...), "\n"))
}

func epochMillis(instant string) *int64 {
	t, err := time.Parse(time.RFC3339, instant)
	if err != nil {
		panic(err)
	}
	ms := t.UnixMilli()
	return &ms
}

func port(p int) *int { return &p }

// validSeed is a seed whose cached access token stays valid across a test's
// clock window.
func validSeed() *StoredTokens {
	return &StoredTokens{RefreshToken: "rt", AccessToken: "access-token-1", AccessExpiresAt: epochMillis("2026-08-20T02:00:00Z")}
}

func baseConfig() Config {
	return Config{StorePath: "/unused-in-tests", Skew: 5 * time.Minute, Port: port(0), Hostname: "127.0.0.1"}
}

type harness struct {
	clock  *StoppedClock
	fake   *FakeHytale
	store  TokenStore
	config Config
	// The wired container, its leaves the doubles above.
	c *Container
}

type options struct {
	now    string
	seed   *StoredTokens
	store  TokenStore
	config func(*Config)
}

// newHarness sets the container's leaf doubles BEFORE wiring it: the stopped
// clock, the fake upstream behind HandlerTransport, an in-memory store, a quiet
// logger and an instant sleep. Everything else is the production graph.
func newHarness(t *testing.T, o options) *harness {
	t.Helper()
	now := o.now
	if now == "" {
		now = "2026-08-20T00:00:00Z"
	}
	start, err := time.Parse(time.RFC3339, now)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{clock: NewStoppedClock(start), fake: newFakeHytale(), store: o.store, config: baseConfig()}
	if h.store == nil {
		h.store = NewMemoryTokenStore(o.seed)
	}
	if o.config != nil {
		o.config(&h.config)
	}
	config := h.config
	h.c = &Container{
		Config:    &config,
		Clock:     h.clock,
		Transport: HandlerTransport(h.fake),
		Store:     h.store,
		Logger:    silentLogger{},
		Sleep:     func(time.Duration) {}, // no real timers in tests
		Retry:     &RetryPolicy{Retries: DefaultRetryPolicy.Retries, BaseDelay: DefaultRetryPolicy.BaseDelay, Sleep: func(context.Context, time.Duration) error { return nil }},
	}
	if err := h.c.Wire(); err != nil {
		t.Fatal(err)
	}
	return h
}

// serve runs one request through a handler, in memory.
func serve(handler http.Handler, method, target, body string, headers ...string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// assertJSON compares two JSON documents by value.
func assertJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("got is not JSON: %v: %q", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not JSON: %v: %q", err, want)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("JSON differs:\n got: %s\nwant: %s", got, want)
	}
}

func assertEqual[T comparable](t *testing.T, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

// upstreamError is err as an UpstreamError, failing the test when it is not
// one.
func upstreamError(t *testing.T, err error) *UpstreamError {
	t.Helper()
	var upstream *UpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("want an UpstreamError, got %v", err)
	}
	return upstream
}
