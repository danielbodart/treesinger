package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Every HTTP surface in Treesinger speaks the standard library's two
// interfaces: its own surface is an http.Handler, and the upstream client is an
// http.RoundTripper. gokit's httpx.HandlerTransport joins the two, so the
// whole system is trivially testable: the fake upstream is just another
// Handler, served in memory in place of http.DefaultTransport. What the client
// does around every request - naming itself, retrying, logging - is httpx's
// decorators, applied in container.go.

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

// cancelled is whether err is the request's own context being cancelled: a
// caller that hung up, which is no failure of the broker's or upstream's.
func cancelled(request *http.Request, err error) bool {
	return errors.Is(err, context.Canceled) && request.Context().Err() != nil
}
