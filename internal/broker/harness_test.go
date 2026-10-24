package broker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/gokit/logx"
)

func epochMillis(instant string) int64 {
	t, err := time.Parse(time.RFC3339, instant)
	if err != nil {
		panic(err)
	}
	return t.UnixMilli()
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
	clock *clock.Stopped
	// Every wait the graph asks for - a device-flow poll's, a retry's backoff
	// - released at once and recorded.
	waits  *clock.Instant
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
// clock, the fake upstream behind httpx.HandlerTransport, an in-memory store,
// a quiet logger and an instant clock for every wait. Everything else is the
// production graph.
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
	h := &harness{clock: clock.NewStopped(start), waits: clock.NewInstant(start), fake: newFakeHytale(), store: o.store, config: baseConfig()}
	if h.store == nil {
		h.store = NewMemoryTokenStore(o.seed)
	}
	if o.config != nil {
		o.config(&h.config)
	}
	config := h.config
	retry := httpx.DefaultRetryPolicy
	retry.Clock = h.waits
	h.c = &Container{
		Config:    &config,
		Clock:     h.clock,
		Transport: httpx.HandlerTransport(h.fake),
		Store:     h.store,
		Logger:    logx.Discard,
		// No real timers in tests, and no stopped clock to advance at each wait.
		Sleep: func(ctx context.Context, d time.Duration) error { return clock.Sleep(ctx, h.waits, d) },
		Retry: &retry,
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
