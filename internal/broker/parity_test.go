package broker

// Where the TypeScript original's loose JavaScript reading of a request, an
// upstream answer, the token file or the environment differs from what Go's
// standard library would do by default, these pin the port to the original.

import (
	"bufio"
	"context"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRequestPathIsNormalisedAsBunsURLParserDoes(t *testing.T) {
	for target, want := range map[string]string{
		"/v1/session":          "/v1/session",
		"/v1/./session":        "/v1/session",
		"/v1/%2e/session":      "/v1/session",
		"/v1/%2E/session":      "/v1/session",
		"/x/../v1/session":     "/v1/session",
		"/x/%2e%2E/v1/session": "/v1/session",
		"/x/.%2e/v1/session":   "/v1/session",
		`/v1\session`:          "/v1/session",
		"/v1/session/.":        "/v1/session/",
		"/v1/session/x/..":     "/v1/session/",
		"/../../v1/session":    "/v1/session",
		"/v1//session":         "/v1//session",
		"/v1/session?x=1":      "/v1/session",
		"/v1/%5Csession":       "/v1/%5Csession",
		"/":                    "/",
	} {
		request := httptest.NewRequest(http.MethodPost, "http://broker/", nil)
		request.RequestURI = target
		assertEqual(t, requestPath(request), want)
	}

	t.Run("over a real connection, dot segments and backslashes reach the gated mint", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed(), config: func(c *Config) { c.BearerToken = "tok"; c.OwnerUUID = "x" }})
		h.fake.on(routeNewSession, scripted{json: fixture("game-session.json")})
		server := httptest.NewServer(h.c.App)
		t.Cleanup(server.Close)

		for _, target := range []string{"/v1/./session", "/v1/%2e/session", "/x/../v1/session", `/v1\session`} {
			assertEqual(t, rawStatus(t, server, "POST "+target+" HTTP/1.1\r\nHost: b\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"), 401)
			assertEqual(t, rawStatus(t, server, "POST "+target+" HTTP/1.1\r\nHost: b\r\nAuthorization: Bearer tok\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"), 200)
		}
	})
}

// rawStatus sends a request exactly as written, since an http.Client would
// clean its path first.
func rawStatus(t *testing.T, server *httptest.Server, request string) int {
	t.Helper()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, request)
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return response.StatusCode
}

func TestUpstreamErrorField(t *testing.T) {
	mint := func(t *testing.T, status int, body string) *httptest.ResponseRecorder {
		h := newHarness(t, options{seed: validSeed(), config: func(c *Config) { c.OwnerUUID = "x" }})
		h.fake.on(routeNewSession, scripted{status: status, json: body})
		return serve(h.c.App, http.MethodPost, brokerSession, "")
	}

	t.Run("is read by its exact key, as body?.error reads it", func(t *testing.T) {
		assertJSON(t, mint(t, 403, `{"Error":"session_limit_exceeded"}`).Body.String(), `{"error":"unknown"}`)
		assertJSON(t, mint(t, 403, `{"ERROR":"x"}`).Body.String(), `{"error":"unknown"}`)
		assertJSON(t, mint(t, 403, `{"error":"a","Error":"b"}`).Body.String(), `{"error":"a"}`)
	})

	t.Run("passes through when it is not a string", func(t *testing.T) {
		res := mint(t, 403, `{"error":5}`)
		assertEqual(t, res.Code, 403)
		assertEqual(t, res.Body.String(), `{"error":5}`)
		assertEqual(t, mint(t, 403, `{"error":{"reason":"x"}}`).Body.String(), `{"error":{"reason":"x"}}`)
		assertJSON(t, mint(t, 403, `{"error":null}`).Body.String(), `{"error":"unknown"}`)
	})

	t.Run("its message is String() of it", func(t *testing.T) {
		for body, want := range map[string]string{
			`{"error":5}`:              "upstream 403: 5",
			`{"error":{"reason":"x"}}`: "upstream 403: [object Object]",
			`{"error":[1,null,"a"]}`:   "upstream 403: 1,,a",
			`{"error":true}`:           "upstream 403: true",
		} {
			err := ok(&http.Response{StatusCode: 403, Status: "403 Forbidden", Body: io.NopCloser(strings.NewReader(body))}, nil)
			assertEqual(t, err.Error(), want)
		}
	})

	t.Run("a device poll fails with it too", func(t *testing.T) {
		h := newHarness(t, options{})
		h.fake.on(routeDeviceToken, scripted{status: 403, json: `{"error":5}`})
		_, err := h.c.OAuth.PollToken(context.Background(), "dev", 0)
		upstream := upstreamError(t, err)
		assertEqual(t, upstream.Code, "5")
		assertEqual(t, upstream.Message, "device token poll failed: 5")
	})
}

func TestUpstreamReasonPhraseIsTheOneSent(t *testing.T) {
	response := func(status string) *http.Response {
		return &http.Response{StatusCode: 503, Status: status, Body: io.NopCloser(strings.NewReader("<html>"))}
	}
	assertEqual(t, ok(response("503 Origin Is Unreachable"), nil).Error(), "upstream 503: Origin Is Unreachable")
	assertEqual(t, ok(response("503"), nil).Error(), "upstream 503: Service Unavailable")
	assertEqual(t, ok(response(""), nil).Error(), "upstream 503: Service Unavailable")
}

func TestMintedSessionIsUpstreamsOwnJSON(t *testing.T) {
	for _, body := range []string{
		`null`,
		`{"sessionToken":"a","identityToken":"b","expiresAt":123}`,
		`{"sessionToken":"a","identityToken":"b","expiresAt":"2026-08-20T01:00:00Z","extra":{"kept":true}}`,
		`{"SessionToken":"X","IDENTITYTOKEN":"Y"}`,
	} {
		h := newHarness(t, options{seed: validSeed(), config: func(c *Config) { c.OwnerUUID = "x" }})
		h.fake.on(routeNewSession, scripted{json: body})
		res := serve(h.c.App, http.MethodPost, brokerSession, "")
		assertEqual(t, res.Code, 200)
		assertEqual(t, res.Body.String(), body)
	}

	t.Run("its typed fields are read by their exact keys", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed(), config: func(c *Config) { c.OwnerUUID = "x" }})
		h.fake.on(routeNewSession, scripted{json: `{"SessionToken":"X","identityToken":"Y"}`})
		session, err := h.c.Sessions.NewSession(context.Background(), "")
		assertEqual(t, err, nil)
		assertEqual(t, session.SessionToken, "")
		assertEqual(t, session.IdentityToken, "Y")
	})
}

func TestDefaultProfileFromAnOddProfilesAnswer(t *testing.T) {
	mint := func(t *testing.T, profiles string) (*httptest.ResponseRecorder, *harness) {
		h := newHarness(t, options{seed: validSeed()})
		h.fake.on(routeProfiles, scripted{json: profiles}).on(routeNewSession, scripted{json: fixture("game-session.json")})
		return serve(h.c.App, http.MethodPost, brokerSession, ""), h
	}

	t.Run("no profiles list is a broker fault, as the TypeScript's TypeError is", func(t *testing.T) {
		for _, body := range []string{`{}`, `null`, `{"profiles":null}`} {
			res, _ := mint(t, body)
			assertEqual(t, res.Code, 500)
			assertJSON(t, res.Body.String(), `{"error":"internal"}`)
		}
	})

	t.Run("an empty list, or a null first profile, is no_profile", func(t *testing.T) {
		for _, body := range []string{`{"profiles":[]}`, `{"profiles":[null]}`} {
			res, _ := mint(t, body)
			assertEqual(t, res.Code, 404)
			assertJSON(t, res.Body.String(), `{"error":"no_profile"}`)
		}
	})

	t.Run("the first profile's uuid is posted as it came, or left out when missing", func(t *testing.T) {
		for profiles, posted := range map[string]string{
			`{"profiles":[{"username":"u"}]}`: `{}`,
			`{"profiles":[{"uuid":5}]}`:       `{"uuid":5}`,
			`{"profiles":[{"UUID":"x"}]}`:     `{}`,
		} {
			res, h := mint(t, profiles)
			assertEqual(t, res.Code, 200)
			call, _ := h.fake.find("/game-session/new")
			assertEqual(t, call.body, posted)
		}
	})
}

func TestTokenResponseIsReadLeniently(t *testing.T) {
	ctx := context.Background()
	stale := &StoredTokens{RefreshToken: "refresh-token-1", AccessExpiresAt: epochMillis("2026-08-20T00:02:00Z")}

	t.Run("a string expires_in is a number, as expires_in * 1000 makes it", func(t *testing.T) {
		h := newHarness(t, options{seed: stale})
		h.fake.on(routeRefresh, scripted{json: `{"access_token":"a2","expires_in":"3600","refresh_token":"r2"}`})
		token, err := h.c.OAuth.EnsureAccessToken(ctx)
		assertEqual(t, err, nil)
		assertEqual(t, token, "a2")
		stored, _ := h.store.Read()
		assertEqual(t, stored.RefreshToken, "r2")
		assertEqual(t, *stored.AccessExpiresAt, *epochMillis("2026-08-20T01:00:00Z"))
	})

	t.Run("a spent refresh is persisted whatever its other fields hold", func(t *testing.T) {
		h := newHarness(t, options{seed: stale})
		h.fake.on(routeRefresh, scripted{json: `{"access_token":7,"expires_in":{"no":1},"refresh_token":"r2"}`})
		token, err := h.c.OAuth.EnsureAccessToken(ctx)
		assertEqual(t, err, nil)
		assertEqual(t, token, "7")
		stored, _ := h.store.Read()
		assertEqual(t, stored.RefreshToken, "r2")
		assertEqual(t, stored.AccessExpiresAt == nil, true) // NaN: refreshed on next use
	})

	t.Run("the device flow stores it too", func(t *testing.T) {
		h := newHarness(t, options{})
		h.fake.on(routeDeviceToken, scripted{json: `{"access_token":"a","expires_in":"60","refresh_token":"r"}`})
		stored, err := h.c.OAuth.PollToken(ctx, "dev", 0)
		assertEqual(t, err, nil)
		assertEqual(t, stored.RefreshToken, "r")
	})

	t.Run("a body that is not an object still fails", func(t *testing.T) {
		h := newHarness(t, options{seed: stale})
		h.fake.on(routeRefresh, scripted{json: `null`})
		_, err := h.c.OAuth.EnsureAccessToken(ctx)
		if err == nil {
			t.Error("want an error")
		}
	})
}

func TestDeviceFlowSlowDownBacksOff(t *testing.T) {
	var slept []time.Duration
	h := newHarness(t, options{})
	h.c.OAuth.Sleep = func(d time.Duration) { slept = append(slept, d) }
	slow := scripted{status: 400, json: `{"error":"slow_down"}`}
	h.fake.on(routeDeviceToken, slow, slow, scripted{json: fixture("token.json")})

	if _, err := h.c.OAuth.PollToken(context.Background(), "dev", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, len(slept), 3)
	assertEqual(t, slept[0], 5*time.Second)
	assertEqual(t, slept[1], 10*time.Second)
	assertEqual(t, slept[2], 15*time.Second)
}

func TestTokenFileIsReadAsJSONParseReadsIt(t *testing.T) {
	read := func(t *testing.T, content string) (*StoredTokens, error) {
		path := filepath.Join(t.TempDir(), "tokens.json")
		os.WriteFile(path, []byte(content), 0o600)
		return NewFileTokenStore(path).Read()
	}

	for content, want := range map[string]int64{
		`{"refresh_token":"rt","access_expires_at":1729700000000.5}`: 1729700000001,
		`{"refresh_token":"rt","access_expires_at":1.7297e12}`:       1729700000000,
		`{"refresh_token":"rt","access_expires_at":"1729700000000"}`: 1729700000000,
	} {
		tokens, err := read(t, content)
		assertEqual(t, err, nil)
		assertEqual(t, tokens.RefreshToken, "rt")
		assertEqual(t, *tokens.AccessExpiresAt, want)
	}

	t.Run("an unusable expiry is stale, not an error", func(t *testing.T) {
		tokens, err := read(t, `{"refresh_token":"rt","access_token":"a","access_expires_at":"soon"}`)
		assertEqual(t, err, nil)
		assertEqual(t, tokens.AccessExpiresAt == nil, true)
	})

	t.Run("keys match exactly", func(t *testing.T) {
		tokens, _ := read(t, `{"Refresh_Token":"rt"}`)
		assertEqual(t, tokens.RefreshToken, "")
	})

	t.Run("a non-object is a login with no refresh token: 401, not 500", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tokens.json")
		os.WriteFile(path, []byte(`[]`), 0o600)
		h := newHarness(t, options{store: NewFileTokenStore(path), config: func(c *Config) { c.OwnerUUID = "x" }})
		res := serve(h.c.App, http.MethodPost, brokerSession, "")
		assertEqual(t, res.Code, 401)
		assertJSON(t, res.Body.String(), `{"error":"not_logged_in"}`)
	})

	t.Run("null is no login", func(t *testing.T) {
		tokens, err := read(t, `null`)
		assertEqual(t, tokens == nil, true)
		assertEqual(t, err, nil)
	})

	t.Run("a file that is not JSON is still an error", func(t *testing.T) {
		_, err := read(t, `{`)
		if err == nil {
			t.Error("want an error")
		}
	})
}

func TestMintRequestBody(t *testing.T) {
	t.Run("a leading byte-order mark is dropped, as request.text() drops it", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed()})
		h.fake.on(routeNewSession, scripted{json: fixture("game-session.json")})
		res := serve(h.c.App, http.MethodPost, brokerSession, "\xef\xbb\xbf{\"uuid\":\"x\"}")
		assertEqual(t, res.Code, 200)
		call, _ := h.fake.find("/game-session/new")
		assertEqual(t, call.body, `{"uuid":"x"}`)
	})

	t.Run("an oversized body is refused with 413, not buffered", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed()})
		res := serve(h.c.App, http.MethodPost, brokerSession, strings.Repeat(" ", maxBodyBytes+1))
		assertEqual(t, res.Code, 413)
	})

	t.Run("a mint whose client hung up logs no failure", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		logger := &recordingLogger{}
		config := baseConfig()
		config.OwnerUUID = "x"
		c := &Container{
			Config: &config, Clock: NewStoppedClock(time.Now()), Logger: logger, Sleep: func(time.Duration) {},
			Store: NewMemoryTokenStore(&StoredTokens{RefreshToken: "rt", AccessToken: "a", AccessExpiresAt: epochMillis("2999-01-01T00:00:00Z")}),
			Transport: RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
				cancel() // the client goes away while upstream is being asked
				return nil, r.Context().Err()
			}),
		}
		if err := c.Wire(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, brokerSession, nil).WithContext(ctx)
		c.App.ServeHTTP(httptest.NewRecorder(), request)
		for _, line := range logger.lines {
			if strings.HasPrefix(line, "error:") {
				t.Errorf("logged %q", line)
			}
		}
	})
}

func TestUpstreamRequestsNameTreesinger(t *testing.T) {
	var seen http.Header
	fake := newFakeHytale().on(routeNewSession, scripted{json: fixture("game-session.json")})
	config := baseConfig()
	config.OwnerUUID = "x"
	c := &Container{
		Config: &config, Clock: NewStoppedClock(time.Now()), Logger: silentLogger{}, Sleep: func(time.Duration) {},
		Store: NewMemoryTokenStore(&StoredTokens{RefreshToken: "rt", AccessToken: "a", AccessExpiresAt: epochMillis("2999-01-01T00:00:00Z")}),
		Transport: RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			seen = r.Header.Clone()
			return HandlerTransport(fake).RoundTrip(r)
		}),
	}
	if err := c.Wire(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Sessions.NewSession(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, seen.Get("User-Agent"), "treesinger/"+Version)
	assertEqual(t, seen.Get("Accept"), "*/*")
}

func TestServerTimesOutIdleConnections(t *testing.T) {
	_, listening := broker(t, brokerOptions{port: port(0)})
	server := listening.servers[0]
	assertEqual(t, server.IdleTimeout, 10*time.Second)
	assertEqual(t, server.ReadHeaderTimeout, 10*time.Second)
}

func TestConfigNumbersReadAsNumberDoes(t *testing.T) {
	t.Run("a port in any form Number() reads", func(t *testing.T) {
		for value, want := range map[string]int{"8080": 8080, "8080.0": 8080, "1e3": 1000, "0x1F90": 8080, "+8080": 8080, " 9090 ": 9090} {
			config, err := ConfigFromEnv(Env{"TREESINGER_PORT": value, "TREESINGER_STORE": "/s"})
			assertEqual(t, err, nil)
			assertEqual(t, *config.Port, want)
		}
		for _, value := range []string{"80.5", "70000", "-1", "http", "0x", "1_000", "Infinity", "NaN"} {
			if _, err := ConfigFromEnv(Env{"TREESINGER_PORT": value, "TREESINGER_STORE": "/s"}); err == nil {
				t.Errorf("port %q: want an error", value)
			}
		}
	})

	t.Run("a skew that is not finite, or overflows a duration, fails at startup", func(t *testing.T) {
		for _, value := range []string{"NaN", "nan", "Inf", "inf", "Infinity", "-Infinity", "1e300", "abc"} {
			if _, err := ConfigFromEnv(Env{"TREESINGER_SKEW_MS": value, "TREESINGER_STORE": "/s"}); err == nil {
				t.Errorf("skew %q: want an error", value)
			}
		}
		config, err := ConfigFromEnv(Env{"TREESINGER_SKEW_MS": "0x10", "TREESINGER_STORE": "/s"})
		assertEqual(t, err, nil)
		assertEqual(t, config.Skew, 16*time.Millisecond)
	})
}

func TestStoreWithoutHomeIsNeverRelative(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	os.Unsetenv("XDG_CONFIG_HOME")
	config, err := ConfigFromEnv(Env{})
	if err != nil {
		return // no passwd entry either: refusing is the other right answer
	}
	assertEqual(t, filepath.IsAbs(config.StorePath), true)
}

func TestJSNumber(t *testing.T) {
	for text, want := range map[string]float64{"": 0, "  ": 0, "12": 12, "1.5": 1.5, ".5": 0.5, "5.": 5, "-1e3": -1000, "0b101": 5, "0o17": 15, "017": 17} {
		assertEqual(t, jsNumber(text), want)
	}
	for _, text := range []string{"x", "1_0", "-0x10", "0x", "inf", "nan", "1e", "--1"} {
		if !math.IsNaN(jsNumber(text)) {
			t.Errorf("%q: want NaN, got %v", text, jsNumber(text))
		}
	}
}
