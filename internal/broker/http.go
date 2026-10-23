package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Every HTTP surface in Treesinger speaks the standard library's two
// interfaces: its own surface is an http.Handler, and the upstream client is an
// http.RoundTripper. HandlerTransport joins the two, so the whole system is
// trivially testable: the fake upstream is just another Handler, served in
// memory in place of http.DefaultTransport.

// RoundTripperFunc is to http.RoundTripper what http.HandlerFunc is to
// http.Handler.
type RoundTripperFunc func(*http.Request) (*http.Response, error)

func (f RoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// Decorator wraps a RoundTripper in another of the same shape.
type Decorator func(http.RoundTripper) http.RoundTripper

// Logger is the slice of a console we use; a capturing double satisfies it.
type Logger interface {
	Log(args ...any)
	Warn(args ...any)
	Error(args ...any)
}

type consoleLogger struct{}

// ConsoleLogger logs to stdout, and warnings and errors to stderr.
func ConsoleLogger() Logger { return consoleLogger{} }

func (consoleLogger) Log(args ...any)   { fmt.Fprintln(os.Stdout, args...) }
func (consoleLogger) Warn(args ...any)  { fmt.Fprintln(os.Stderr, args...) }
func (consoleLogger) Error(args ...any) { fmt.Fprintln(os.Stderr, args...) }

// UpstreamError is a non-2xx (or otherwise unusable) upstream response,
// carrying the HTTP status so callers/handlers can map it back out: 401 →
// re-auth needed, 403 → entitlement/session-limit. Code is the upstream `error`
// field when present.
type UpstreamError struct {
	Status  int
	Code    string
	Message string
	// Value is the upstream `error` exactly as sent, when there was one. It is
	// what the client is answered with, so a code that is not a string (a
	// number, an object) passes through as it does in the TypeScript, where
	// Code is String() of it.
	Value json.RawMessage
}

func (e *UpstreamError) Error() string { return e.Message }

// upstreamFailure is the UpstreamError for a non-2xx response whose body was
// data: `upstream <status>: <error, else the reason phrase>`.
func upstreamFailure(response *http.Response, data []byte) *UpstreamError {
	value, found := errorField(data)
	if !found {
		return &UpstreamError{Status: response.StatusCode, Code: "unknown", Message: fmt.Sprintf("upstream %d: %s", response.StatusCode, reasonPhrase(response))}
	}
	code := jsonText(value)
	return &UpstreamError{Status: response.StatusCode, Code: code, Value: value, Message: fmt.Sprintf("upstream %d: %s", response.StatusCode, code)}
}

// reasonPhrase is the status text upstream actually sent ("503 Origin Is
// Unreachable"), as fetch's statusText is, falling back to the canonical one
// when it sent none.
func reasonPhrase(response *http.Response) string {
	phrase := strings.TrimSpace(strings.TrimPrefix(response.Status, strconv.Itoa(response.StatusCode)))
	if phrase == "" {
		return http.StatusText(response.StatusCode)
	}
	return phrase
}

// RetryPolicy says how often, and after how long, Retrying tries again.
type RetryPolicy struct {
	Retries   int
	BaseDelay time.Duration
	// Waits out a backoff, or gives up when the request is cancelled. A real
	// timer when nil.
	Sleep func(ctx context.Context, d time.Duration) error
}

// DefaultRetryPolicy is two retries, 200ms then 400ms apart.
var DefaultRetryPolicy = RetryPolicy{Retries: 2, BaseDelay: 200 * time.Millisecond}

// Retrying retries transient upstream failures (transport errors, 429, and
// 5xx) with exponential backoff. The body is buffered once and each attempt
// sends a fresh copy, so a retry can resend a body-bearing POST (a request body
// is a single-use stream).
//
// Note on the rotating refresh token: a refresh that the server processed but
// whose response was lost would, on retry, fail with 401 and surface as
// re-auth-needed (the documented recovery), never a silent double-spend.
func Retrying(policy RetryPolicy) Decorator {
	sleep := policy.Sleep
	if sleep == nil {
		sleep = delay
	}
	return func(inner http.RoundTripper) http.RoundTripper {
		return RoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			var body []byte
			if request.Body != nil && request.Body != http.NoBody {
				buffered, err := io.ReadAll(request.Body)
				request.Body.Close()
				if err != nil {
					return nil, err
				}
				body = buffered
			}
			build := func() *http.Request {
				attempt := request.Clone(request.Context())
				if body != nil {
					attempt.Body = io.NopCloser(bytes.NewReader(body))
					attempt.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
					attempt.ContentLength = int64(len(body))
				}
				return attempt
			}
			for attempt := 0; ; attempt++ {
				response, err := inner.RoundTrip(build())
				transient := err != nil || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
				if !transient || attempt >= policy.Retries {
					return response, err
				}
				if response != nil {
					// Drained so the connection goes back to the pool.
					io.Copy(io.Discard, response.Body)
					response.Body.Close()
				}
				if err := sleep(request.Context(), policy.BaseDelay<<attempt); err != nil {
					return nil, err
				}
			}
		})
	}
}

// Logging logs method + URL + status + duration around each call.
//
// Deviations, accepted: this and Retrying sit under http.Client, which follows
// a redirect by calling its transport once per hop, so a redirect logs (and
// retries) each hop where fetch, following redirects inside the call, logs one
// line for the whole chain; Hytale's endpoints do not redirect. And an error is
// logged as its one-line Error() text, not Bun's multi-line inspection of it.
func Logging(logger Logger) Decorator {
	return func(inner http.RoundTripper) http.RoundTripper {
		return RoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			started := time.Now()
			response, err := inner.RoundTrip(request)
			if err != nil {
				// A caller that hung up is not an upstream fault, and is
				// nothing the TypeScript (which never cancels) would log.
				if !cancelled(request, err) {
					logger.Error(fmt.Sprintf("%s %s → threw", request.Method, request.URL), err)
				}
				return nil, err
			}
			elapsed := time.Since(started).Round(time.Millisecond).Milliseconds()
			logger.Log(fmt.Sprintf("%s %s → %d (%dms)", request.Method, request.URL, response.StatusCode, elapsed))
			return response, nil
		})
	}
}

// cancelled is whether err is the request's own context being cancelled.
func cancelled(request *http.Request, err error) bool {
	return errors.Is(err, context.Canceled) && request.Context().Err() != nil
}

// Identifying names Treesinger to upstream, as `treesinger/<version>`, rather
// than leaving it to the runtime's default (Bun's fetch sends `Bun/<version>`,
// Go's `Go-http-client/1.1`), and asks for any type, as fetch does by
// default. Headers a request already carries are left alone.
func Identifying(version string) Decorator {
	return func(inner http.RoundTripper) http.RoundTripper {
		return RoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			if request.Header.Get("User-Agent") != "" && request.Header.Get("Accept") != "" {
				return inner.RoundTrip(request)
			}
			// A RoundTripper must not change the request it was given.
			identified := request.Clone(request.Context())
			if identified.Header.Get("User-Agent") == "" {
				identified.Header.Set("User-Agent", "treesinger/"+version)
			}
			if identified.Header.Get("Accept") == "" {
				identified.Header.Set("Accept", "*/*")
			}
			return inner.RoundTrip(identified)
		})
	}
}

// The one real timer in the codebase; only ever reached on a retry backoff.
func delay(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ok decodes a 2xx JSON body into `into`, or returns an UpstreamError carrying
// the status. The body is always consumed and closed.
func ok(response *http.Response, into any) error {
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if succeeded(response) {
		return json.Unmarshal(data, into)
	}
	return upstreamFailure(response, data)
}

// errorField is `body?.error`: the value under exactly the key "error" (not
// "Error", which encoding/json's struct matching would also take), of any
// type, tolerating a body that is missing, not JSON or not an object. A null
// one counts as missing, as it does for `??`.
func errorField(data []byte) (json.RawMessage, bool) {
	object, isObject := jsonObject(data)
	if !isObject || !present(object["error"]) {
		return nil, false
	}
	return object["error"], true
}

func succeeded(response *http.Response) bool {
	return response.StatusCode >= 200 && response.StatusCode < 300
}

// marshal is JSON.stringify: compact, and with no HTML escaping.
//
// Deviation, accepted: encoding/json always writes U+2028 and U+2029 as
// \u2028 and \u2029, and invalid UTF-8 as U+FFFD, where JSON.stringify writes
// the raw characters. The documents mean the same; only the bytes differ.
func marshal(v any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}
