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
	Error(args ...any)
}

type consoleLogger struct{}

// ConsoleLogger logs to stdout, and errors to stderr.
func ConsoleLogger() Logger { return consoleLogger{} }

func (consoleLogger) Log(args ...any)   { fmt.Fprintln(os.Stdout, args...) }
func (consoleLogger) Error(args ...any) { fmt.Fprintln(os.Stderr, args...) }

// UpstreamError is a non-2xx (or otherwise unusable) upstream response,
// carrying the HTTP status so callers/handlers can map it back out: 401 →
// re-auth needed, 403 → entitlement/session-limit. Code is the upstream `error`
// field when present.
type UpstreamError struct {
	Status  int
	Code    string
	Message string
}

func (e *UpstreamError) Error() string { return e.Message }

// upstreamFailure is the UpstreamError for a non-2xx response whose body was
// data: `upstream <status>: <error, else the status text>`.
func upstreamFailure(status int, data []byte) *UpstreamError {
	code := errorCode(data)
	detail := code
	if code == "unknown" {
		detail = http.StatusText(status)
	}
	return &UpstreamError{Status: status, Code: code, Message: fmt.Sprintf("upstream %d: %s", status, detail)}
}

// errorCode is the `error` string of an OAuth-style error body, or "unknown"
// when there is none to read.
func errorCode(data []byte) string {
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &body) != nil || body.Error == "" {
		return "unknown"
	}
	return body.Error
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
			body, err := buffer(request)
			if err != nil {
				return nil, err
			}
			for attempt := 0; ; attempt++ {
				response, err := inner.RoundTrip(withBody(request, body))
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

// buffer reads and closes a request's body, nil when it has none.
func buffer(request *http.Request) ([]byte, error) {
	if request.Body == nil || request.Body == http.NoBody {
		return nil, nil
	}
	defer request.Body.Close()
	return io.ReadAll(request.Body)
}

// withBody is a copy of request carrying its own reader over body.
func withBody(request *http.Request, body []byte) *http.Request {
	attempt := request.Clone(request.Context())
	if body != nil {
		attempt.Body = io.NopCloser(bytes.NewReader(body))
		attempt.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		attempt.ContentLength = int64(len(body))
	}
	return attempt
}

// Logging logs method + URL + status + duration around each call. Under
// http.Client it sees each redirect hop as its own call; Hytale's endpoints do
// not redirect.
func Logging(logger Logger) Decorator {
	return func(inner http.RoundTripper) http.RoundTripper {
		return RoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			started := time.Now()
			response, err := inner.RoundTrip(request)
			if err != nil {
				// A caller that hung up is not an upstream fault.
				if !cancelled(request, err) {
					logger.Error(fmt.Sprintf("%s %s → threw", request.Method, request.URL), err)
				}
				return nil, err
			}
			logger.Log(fmt.Sprintf("%s %s → %d (%dms)", request.Method, request.URL, response.StatusCode, time.Since(started).Milliseconds()))
			return response, nil
		})
	}
}

// cancelled is whether err is the request's own context being cancelled.
func cancelled(request *http.Request, err error) bool {
	return errors.Is(err, context.Canceled) && request.Context().Err() != nil
}

// Identifying names Treesinger to upstream as `treesinger/<version>`, rather
// than as Go's generic `Go-http-client/1.1`.
func Identifying(version string) Decorator {
	return func(inner http.RoundTripper) http.RoundTripper {
		return RoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			if request.Header.Get("User-Agent") != "" {
				return inner.RoundTrip(request)
			}
			// A RoundTripper must not change the request it was given.
			identified := request.Clone(request.Context())
			identified.Header.Set("User-Agent", "treesinger/"+version)
			return inner.RoundTrip(identified)
		})
	}
}

// The one real timer in the codebase: a retry's backoff, and the wait between
// device-flow polls.
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

// decode reads a 2xx JSON body into `into`, or returns an UpstreamError
// carrying the status. The body is always consumed and closed.
func decode(response *http.Response, into any) error {
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if !succeeded(response) {
		return upstreamFailure(response.StatusCode, data)
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("%s %s: %w", response.Request.Method, response.Request.URL, err)
	}
	return nil
}

func succeeded(response *http.Response) bool {
	return response.StatusCode >= 200 && response.StatusCode < 300
}
