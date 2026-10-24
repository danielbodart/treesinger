package broker

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/danielbodart/gokit/httpx"
)

// startLogin is one POST /v1/login to the admin handler, in memory, read to
// its end.
func startLogin(t *testing.T, ctx context.Context, h *harness) (*http.Response, []string) {
	t.Helper()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, socketURL+"/v1/login", nil)
	response, err := (&http.Client{Transport: httpx.HandlerTransport(h.c.Admin)}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response, strings.Split(strings.TrimSuffix(body(t, response), "\n"), "\n")
}

// scriptLogin has the fake run a device flow to approval, through
// authorization_pending and a slow_down.
func scriptLogin(h *harness) {
	h.fake.on(routeDeviceAuth, scripted{json: fixture("device-auth.json")}).
		on(routeDeviceToken,
			scripted{status: 400, json: fixture("authorization-pending.json")},
			scripted{status: 400, json: `{"error":"slow_down"}`},
			scripted{json: fixture("token.json")})
}

const prompt = `{"verification_uri_complete":"https://accounts.hytale.com/device?user_code=ABCD-1234","verification_uri":"https://accounts.hytale.com/device","user_code":"ABCD-1234"}`

func TestAdminLogin(t *testing.T) {
	t.Run("streams the prompt then ok, and the broker mints for the new login at once", func(t *testing.T) {
		h := newHarness(t, options{})
		scriptLogin(h)
		h.fake.on(routeProfiles, scripted{json: fixture("profiles.json")}).on(routeNewSession, scripted{json: fixture("game-session.json")})
		assertEqual(t, serve(h.c.App, http.MethodPost, brokerSession, "").Code, 401) // before

		response, events := startLogin(t, context.Background(), h)

		assertEqual(t, response.StatusCode, 200)
		assertEqual(t, response.Header.Get("Content-Type"), "application/x-ndjson")
		assertEqual(t, len(events), 2)
		assertJSON(t, events[0], prompt)
		assertJSON(t, events[1], `{"ok":true}`)
		slept := h.waits.Waits()
		assertEqual(t, len(slept), 3)
		assertEqual(t, slept[2], 10*time.Second) // slow_down backed off
		stored, _ := h.store.Read()
		assertEqual(t, stored.RefreshToken, "refresh-token-1")
		assertEqual(t, serve(h.c.App, http.MethodPost, brokerSession, "").Code, 200) // after, with no restart
	})

	for name, code := range map[string]string{"expires": "expired_token", "is denied": "access_denied"} {
		t.Run("a login that "+name+" ends in an error event, and stores nothing", func(t *testing.T) {
			h := newHarness(t, options{})
			h.fake.on(routeDeviceAuth, scripted{json: fixture("device-auth.json")}).
				on(routeDeviceToken, scripted{status: 400, json: fixture("authorization-pending.json")}, scripted{status: 400, json: `{"error":"` + code + `"}`})

			_, events := startLogin(t, context.Background(), h)

			assertEqual(t, len(events), 2)
			assertJSON(t, events[0], prompt)
			assertJSON(t, events[1], `{"error":"device token poll failed: `+code+`"}`)
			stored, _ := h.store.Read()
			assertEqual(t, stored, nil)
		})
	}

	t.Run("a flow that cannot start is an error event", func(t *testing.T) {
		h := newHarness(t, options{})
		h.fake.on(routeDeviceAuth, scripted{status: 400, json: `{"error":"invalid_client"}`})

		response, events := startLogin(t, context.Background(), h)

		assertEqual(t, response.StatusCode, 200)
		assertEqual(t, len(events), 1)
		assertJSON(t, events[0], `{"error":"upstream 400: invalid_client"}`)
	})

	t.Run("one at a time: another login while one runs is a 409", func(t *testing.T) {
		h := newHarness(t, options{})
		scriptLogin(h)
		polling, release := make(chan struct{}), make(chan struct{})
		h.c.OAuth.Sleep = func(context.Context, time.Duration) error {
			select {
			case polling <- struct{}{}:
				<-release
			default:
			}
			return nil
		}
		first := make(chan []string)
		go func() {
			_, events := startLogin(t, context.Background(), h)
			first <- events
		}()
		<-polling

		second := serve(h.c.Admin, http.MethodPost, "http://broker/v1/login", "")

		assertEqual(t, second.Code, 409)
		assertJSON(t, second.Body.String(), `{"error":"login_in_progress"}`)
		close(release)
		events := <-first
		assertJSON(t, events[len(events)-1], `{"ok":true}`)
		// Done, it frees the next.
		_, events = startLogin(t, context.Background(), h)
		assertJSON(t, events[len(events)-1], `{"ok":true}`)
	})

	t.Run("a login its client abandons frees the next at once", func(t *testing.T) {
		h := newHarness(t, options{})
		scriptLogin(h)
		ctx, cancel := context.WithCancel(context.Background())
		h.c.OAuth.Sleep = func(ctx context.Context, _ time.Duration) error {
			cancel() // the operator gives up while it waits for approval
			<-ctx.Done()
			return ctx.Err()
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, socketURL+"/v1/login", nil)
		(&http.Client{Transport: httpx.HandlerTransport(h.c.Admin)}).Do(request)
		h.c.OAuth.Sleep = h.c.Sleep // the harness's instant waits again

		_, events := startLogin(t, context.Background(), h)

		assertJSON(t, events[len(events)-1], `{"ok":true}`)
	})

	t.Run("a new login forgets the old one's profiles", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed()})
		h.fake.on(routeProfiles, scripted{json: fixture("profiles.json")}).on(routeNewSession, scripted{json: fixture("game-session.json")})
		scriptLogin(h)
		serve(h.c.App, http.MethodPost, brokerSession, "")
		serve(h.c.App, http.MethodPost, brokerSession, "")
		assertEqual(t, h.fake.countOf(routeProfiles), 1) // cached

		startLogin(t, context.Background(), h)
		serve(h.c.App, http.MethodPost, brokerSession, "")

		assertEqual(t, h.fake.countOf(routeProfiles), 2)
	})

	t.Run("is the admin handler's alone: the app has no /v1/login", func(t *testing.T) {
		h := newHarness(t, options{})
		assertEqual(t, serve(h.c.App, http.MethodPost, "http://broker/v1/login", "").Code, 404)
		assertEqual(t, h.fake.countOf(routeDeviceAuth), 0)
	})

	t.Run("the admin handler serves nothing else", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed()})
		assertEqual(t, serve(h.c.Admin, http.MethodGet, "http://broker/health", "").Code, 404)
		assertEqual(t, serve(h.c.Admin, http.MethodPost, brokerSession, "").Code, 404)
		assertEqual(t, serve(h.c.Admin, http.MethodGet, "http://broker/v1/login", "").Code, 404)
	})
}

func TestLoginWaitsForARefreshInFlight(t *testing.T) {
	// A refresh that started on the old login, still in flight when the new
	// one completes, must not land after it and put the old tokens back.
	h := newHarness(t, options{seed: &StoredTokens{RefreshToken: "old-refresh", AccessExpiresAt: epochMillis("2026-08-20T00:02:00Z")}})
	scriptLogin(h)
	refreshing, release := make(chan struct{}), make(chan struct{})
	h.c.Transport = httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/oauth2/token" {
			form, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(form))
			if strings.Contains(string(form), "grant_type=refresh_token") {
				close(refreshing)
				<-release
			}
		}
		return httpx.HandlerTransport(h.fake).RoundTrip(r)
	})
	h.fake.on(routeRefresh, scripted{json: fixture("token-refreshed.json")})
	if err := h.c.Wire(); err != nil {
		t.Fatal(err)
	}
	go h.c.OAuth.EnsureAccessToken(context.Background())
	<-refreshing

	done := make(chan []string)
	go func() {
		_, events := startLogin(t, context.Background(), h)
		done <- events
	}()
	// Until the login has its tokens, and goes to write them.
	for h.fake.countOf(routeDeviceToken) < 3 {
		runtime.Gosched()
	}
	close(release)
	events := <-done

	assertJSON(t, events[len(events)-1], `{"ok":true}`)
	stored, _ := h.store.Read()
	assertEqual(t, stored.RefreshToken, "refresh-token-1") // the login's, written last
}

func TestLoginForgetsProfilesStillBeingFetched(t *testing.T) {
	// A profiles lookup already in flight when a login completes must not
	// cache the old account's profiles after the login has forgotten them.
	h := newHarness(t, options{seed: validSeed()})
	scriptLogin(h)
	h.fake.on(routeProfiles, scripted{json: fixture("profiles.json")}, scripted{json: fixture("profiles.json")}).
		on(routeNewSession, scripted{json: fixture("game-session.json")})
	fetching, release := make(chan struct{}), make(chan struct{})
	var once bool
	h.c.Transport = httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/my-account/get-profiles" && !once {
			once = true
			close(fetching)
			<-release
		}
		return httpx.HandlerTransport(h.fake).RoundTrip(r)
	})
	if err := h.c.Wire(); err != nil {
		t.Fatal(err)
	}
	looked := make(chan error)
	go func() {
		_, err := h.c.Sessions.GetProfiles(context.Background())
		looked <- err
	}()
	<-fetching

	_, events := startLogin(t, context.Background(), h)
	assertJSON(t, events[len(events)-1], `{"ok":true}`)
	close(release)
	if err := <-looked; err != nil {
		t.Fatal(err)
	}

	serve(h.c.App, http.MethodPost, brokerSession, "")
	assertEqual(t, h.fake.countOf(routeProfiles), 2) // fetched again, for the new login
}
