package broker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestHandlerTransport(t *testing.T) {
	t.Run("serves a client's request in memory, as a server would have received it", func(t *testing.T) {
		var seen *http.Request
		var seenBody string
		echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = r
			data, _ := io.ReadAll(r.Body)
			seenBody = string(data)
			w.Header().Set("X-Echo", r.Header.Get("X-Ping"))
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, "pong")
		})
		client := &http.Client{Transport: HandlerTransport(echo)}

		request, _ := http.NewRequest(http.MethodPost, "https://upstream.example/path?q=1", strings.NewReader("ping"))
		request.Header.Set("X-Ping", "hello")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)

		assertEqual(t, seen.Method, http.MethodPost)
		assertEqual(t, seen.Host, "upstream.example")
		assertEqual(t, seen.RequestURI, "/path?q=1")
		assertEqual(t, seen.URL.Path, "/path")
		assertEqual(t, seenBody, "ping")
		assertEqual(t, response.StatusCode, http.StatusCreated)
		assertEqual(t, response.Header.Get("X-Echo"), "hello")
		assertEqual(t, string(data), "pong")
		assertEqual(t, response.Request, request)
	})

	t.Run("a bodiless request reaches the handler with an empty, non-nil body", func(t *testing.T) {
		var body io.ReadCloser
		transport := HandlerTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { body = r.Body }))
		request, _ := http.NewRequest(http.MethodGet, "https://upstream.example/", nil)
		if _, err := transport.RoundTrip(request); err != nil {
			t.Fatal(err)
		}
		if body == nil {
			t.Error("the handler got a nil body")
		}
	})

	t.Run("a panicking handler is a transport error, as a dropped connection would be", func(t *testing.T) {
		transport := HandlerTransport(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
		request, _ := http.NewRequest(http.MethodGet, "https://upstream.example/", nil)
		response, err := transport.RoundTrip(request)
		if response != nil {
			t.Error("want no response")
		}
		assertErrorContains(t, err, "boom")
	})
}

type outcome struct {
	status int // 0 means the transport failed
}

// scriptedTransport answers each attempt with the next outcome, recording the
// body it was sent.
func scriptedTransport(outcomes []outcome, bodies *[]string) http.RoundTripper {
	attempt := 0
	return RoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		sent := ""
		if request.Body != nil {
			data, _ := io.ReadAll(request.Body)
			sent = string(data)
		}
		*bodies = append(*bodies, sent)
		next := outcomes[min(attempt, len(outcomes)-1)]
		attempt++
		if next.status == 0 {
			return nil, errors.New("connection reset")
		}
		return &http.Response{StatusCode: next.status, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})
}

func TestRetrying(t *testing.T) {
	tests := []struct {
		name     string
		outcomes []outcome
		attempts int
		status   int // 0 means it returns the transport's error
		delays   []time.Duration
	}{
		{"success is not retried", []outcome{{200}}, 1, 200, nil},
		{"a 4xx is not retried", []outcome{{404}}, 1, 404, nil},
		{"a 5xx is retried with exponential backoff", []outcome{{503}, {502}, {200}}, 3, 200, []time.Duration{200 * time.Millisecond, 400 * time.Millisecond}},
		{"a 429 is retried", []outcome{{429}, {200}}, 2, 200, []time.Duration{200 * time.Millisecond}},
		{"a transport error is retried", []outcome{{0}, {200}}, 2, 200, []time.Duration{200 * time.Millisecond}},
		{"the last 5xx is returned once the retries run out", []outcome{{500}}, 3, 500, []time.Duration{200 * time.Millisecond, 400 * time.Millisecond}},
		{"the last error is returned once the retries run out", []outcome{{0}}, 3, 0, []time.Duration{200 * time.Millisecond, 400 * time.Millisecond}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var bodies []string
			var delays []time.Duration
			policy := DefaultRetryPolicy
			policy.Sleep = func(_ context.Context, d time.Duration) error {
				delays = append(delays, d)
				return nil
			}
			transport := Retrying(policy)(scriptedTransport(test.outcomes, &bodies))

			request, _ := http.NewRequest(http.MethodPost, "https://upstream.example/", strings.NewReader("grant_type=refresh_token"))
			response, err := transport.RoundTrip(request)

			if test.status == 0 {
				assertErrorContains(t, err, "connection reset")
			} else {
				assertEqual(t, err, nil)
				assertEqual(t, response.StatusCode, test.status)
			}
			assertEqual(t, len(bodies), test.attempts)
			for _, sent := range bodies {
				assertEqual(t, sent, "grant_type=refresh_token") // the body resent in full every time
			}
			assertEqual(t, len(delays), len(test.delays))
			for i := range min(len(delays), len(test.delays)) {
				assertEqual(t, delays[i], test.delays[i])
			}
		})
	}

	t.Run("a cancelled request stops waiting out the backoff", func(t *testing.T) {
		var bodies []string
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		transport := Retrying(DefaultRetryPolicy)(scriptedTransport([]outcome{{503}}, &bodies))
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://upstream.example/", nil)

		_, err := transport.RoundTrip(request)

		assertEqual(t, errors.Is(err, context.Canceled), true)
		assertEqual(t, len(bodies), 1)
	})
}

func TestLogging(t *testing.T) {
	t.Run("logs method, URL, status and duration", func(t *testing.T) {
		logger := &recordingLogger{}
		var bodies []string
		transport := Logging(logger)(scriptedTransport([]outcome{{201}}, &bodies))
		request, _ := http.NewRequest(http.MethodPost, "https://upstream.example/a?b=c", nil)

		transport.RoundTrip(request)

		assertEqual(t, len(logger.lines), 1)
		if !regexp.MustCompile(`^log: POST https://upstream\.example/a\?b=c → 201 \(\d+ms\)$`).MatchString(logger.lines[0]) {
			t.Errorf("unexpected line %q", logger.lines[0])
		}
	})

	t.Run("logs a transport error, and passes it on", func(t *testing.T) {
		logger := &recordingLogger{}
		var bodies []string
		transport := Logging(logger)(scriptedTransport([]outcome{{0}}, &bodies))
		request, _ := http.NewRequest(http.MethodGet, "https://upstream.example/", nil)

		_, err := transport.RoundTrip(request)

		assertErrorContains(t, err, "connection reset")
		assertEqual(t, strings.Join(logger.lines, "\n"), "error: GET https://upstream.example/ → threw connection reset")
	})

	t.Run("the container logs once around all the retries", func(t *testing.T) {
		logger := &recordingLogger{}
		h := newHarness(t, options{seed: validSeed()})
		h.c.Logger = logger
		h.c.Wire()
		h.fake.on(routeProfiles, scripted{status: 503, json: `{}`}, scripted{json: fixture("profiles.json")})

		h.c.Sessions.GetProfiles(context.Background())

		assertEqual(t, h.fake.countOf(routeProfiles), 2)
		assertEqual(t, len(logger.lines), 1) // the 503 was retried inside the one logged call
		assertEqual(t, strings.HasPrefix(logger.lines[0], "log: GET https://account-data.hytale.com/my-account/get-profiles → 200 ("), true)
	})
}

func TestIdentifying(t *testing.T) {
	var seen []string
	inner := RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r.Header.Get("User-Agent"))
		return &http.Response{StatusCode: 200, Body: http.NoBody, Request: r}, nil
	})
	transport := Identifying("1.2.3")(inner)

	plain, _ := http.NewRequest(http.MethodGet, "https://upstream.example/", nil)
	transport.RoundTrip(plain)
	named, _ := http.NewRequest(http.MethodGet, "https://upstream.example/", nil)
	named.Header.Set("User-Agent", "someone-else")
	transport.RoundTrip(named)

	assertEqual(t, seen[0], "treesinger/1.2.3")
	assertEqual(t, seen[1], "someone-else")            // a request's own is left alone
	assertEqual(t, plain.Header.Get("User-Agent"), "") // and the caller's request is not changed
}
