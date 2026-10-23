package broker

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
)

// The fake keys `/oauth2/token` by `grant_type`, since it serves three grants;
// every other endpoint by `METHOD /pathname`.
const (
	routeDeviceAuth     = "POST /oauth2/device/auth"
	routeDeviceToken    = "POST /oauth2/token urn:ietf:params:oauth:grant-type:device_code"
	routeRefresh        = "POST /oauth2/token refresh_token"
	routeProfiles       = "GET /my-account/get-profiles"
	routeNewSession     = "POST /game-session/new"
	routeRefreshSession = "POST /game-session/refresh"
	routeTerminate      = "DELETE /game-session"
)

type scripted struct {
	status int // 200 when zero
	json   string
}

type call struct {
	method, pathname, body, authorization string
}

// FakeHytale is an in-memory stand-in for Hypixel's services: an http.Handler,
// reached through HandlerTransport, that replays recorded response shapes from
// a route table and logs every call, so tests can both script upstream
// behaviour (pending → success, rotation, errors) and assert on how many times
// a route was hit (e.g. exactly one refresh under concurrency).
type FakeHytale struct {
	mu      sync.Mutex
	calls   []call
	scripts map[string][]scripted
}

func newFakeHytale() *FakeHytale { return &FakeHytale{scripts: map[string][]scripted{}} }

// on scripts the responses for a route. Multiple responses are consumed in
// order; the last one repeats for any further calls (so a steady state is one
// entry).
func (f *FakeHytale) on(route string, responses ...scripted) *FakeHytale {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[route] = responses
	return f
}

func (f *FakeHytale) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := ""
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		data, _ := io.ReadAll(r.Body)
		body = string(data)
	}
	route := keyFor(r.Method, r.URL.Path, body)

	f.mu.Lock()
	f.calls = append(f.calls, call{r.Method, r.URL.Path, body, r.Header.Get("Authorization")})
	queue := f.scripts[route]
	if len(queue) == 0 {
		f.mu.Unlock()
		panic(fmt.Sprintf("FakeHytale: no response scripted for '%s'", route))
	}
	next := queue[0]
	if len(queue) > 1 {
		f.scripts[route] = queue[1:]
	}
	f.mu.Unlock()

	status := next.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.WriteString(w, next.json)
}

// countOf is how many times a given route was called.
func (f *FakeHytale) countOf(route string) int {
	count := 0
	for _, c := range f.recorded() {
		if keyFor(c.method, c.pathname, c.body) == route {
			count++
		}
	}
	return count
}

// find is the first call to a pathname.
func (f *FakeHytale) find(pathname string) (call, bool) {
	for _, c := range f.recorded() {
		if c.pathname == pathname {
			return c, true
		}
	}
	return call{}, false
}

func (f *FakeHytale) recorded() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func keyFor(method, pathname, body string) string {
	if pathname == "/oauth2/token" {
		form, _ := url.ParseQuery(body)
		return "POST /oauth2/token " + form.Get("grant_type")
	}
	return method + " " + pathname
}

// fixture is a recorded upstream body, from testdata.
func fixture(name string) string {
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		panic(err)
	}
	return string(data)
}
