package broker

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
)

// HandlerTransport turns an http.Handler into an http.RoundTripper: each
// request is served in memory, with no socket and no port, and the recorded
// response handed back as the upstream's. It is the seam that makes a handler
// and a client the same thing to a test: the fake Hytale upstream is a Handler
// like Treesinger's own.
//
// The request is turned into what a server would have received (a request URI,
// a Host, a non-nil body), and a panicking handler surfaces as a transport
// error, as http.Server turns one into a dropped connection.
func HandlerTransport(handler http.Handler) http.RoundTripper {
	return RoundTripperFunc(func(request *http.Request) (response *http.Response, err error) {
		if request.Body != nil {
			defer request.Body.Close()
		}
		served := request.Clone(request.Context())
		served.RequestURI = request.URL.RequestURI()
		if served.Host == "" {
			served.Host = request.URL.Host
		}
		if served.Body == nil {
			served.Body = http.NoBody
		}
		defer func() {
			if panicked := recover(); panicked != nil {
				response, err = nil, fmt.Errorf("%s %s: handler panicked: %v", request.Method, request.URL, panicked)
			}
		}()
		recorder := &recorder{header: http.Header{}}
		handler.ServeHTTP(recorder, served)
		return recorder.response(request), nil
	})
}

// recorder is the ResponseWriter HandlerTransport serves into. It is
// httptest.ResponseRecorder's job, done here so the production binary does
// not link the testing packages that one brings in.
type recorder struct {
	header  http.Header
	written http.Header // the header as it was when the status was written
	status  int
	body    bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status, r.written = status, r.header.Clone()
	}
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 && r.header.Get("Content-Type") == "" {
		r.header.Set("Content-Type", http.DetectContentType(p))
	}
	r.WriteHeader(http.StatusOK)
	return r.body.Write(p)
}

func (r *recorder) response(request *http.Request) *http.Response {
	r.WriteHeader(http.StatusOK)
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", r.status, http.StatusText(r.status)),
		StatusCode:    r.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        r.written,
		Body:          io.NopCloser(bytes.NewReader(r.body.Bytes())),
		ContentLength: int64(r.body.Len()),
		Request:       request,
	}
}
