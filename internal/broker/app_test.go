package broker

import (
	"encoding/json"
	"net/http"
	"testing"
)

const brokerSession = "http://broker/v1/session"

func TestMintSession(t *testing.T) {
	t.Run("mints and returns the pair", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed()})
		h.fake.on(routeProfiles, scripted{json: fixture("profiles.json")}).on(routeNewSession, scripted{json: fixture("game-session.json")})

		res := serve(h.c.App, http.MethodPost, brokerSession, "")

		assertEqual(t, res.Code, 200)
		assertJSON(t, res.Body.String(), fixture("game-session.json"))
	})

	t.Run("honours a uuid in the body", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed()})
		h.fake.on(routeNewSession, scripted{json: fixture("game-session.json")})

		serve(h.c.App, http.MethodPost, brokerSession, `{"uuid":"cccccccc-cccc-cccc-cccc-cccccccccccc"}`)

		mint, _ := h.fake.find("/game-session/new")
		assertJSON(t, mint.body, `{"uuid":"cccccccc-cccc-cccc-cccc-cccccccccccc"}`)
	})

	t.Run("a malformed body is a 400, not a 500", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed()})
		res := serve(h.c.App, http.MethodPost, brokerSession, "{uuid:")
		assertEqual(t, res.Code, 400)
		assertJSON(t, res.Body.String(), `{"error":"bad_request"}`)
	})

	t.Run("a whitespace body falls back to the default profile", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed(), config: func(c *Config) { c.OwnerUUID = "x" }})
		h.fake.on(routeNewSession, scripted{json: fixture("game-session.json")})
		res := serve(h.c.App, http.MethodPost, brokerSession, "  ")
		assertEqual(t, res.Code, 200)
	})

	t.Run("maps a 403 session-limit through to a 403", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed(), config: func(c *Config) { c.OwnerUUID = "x" }})
		h.fake.on(routeNewSession, scripted{status: 403, json: fixture("error-session-limit.json")})

		res := serve(h.c.App, http.MethodPost, brokerSession, "")

		assertEqual(t, res.Code, 403)
		assertJSON(t, res.Body.String(), `{"error":"session_limit_exceeded"}`)
	})
}

func TestHealthRoute(t *testing.T) {
	t.Run("200 when a mint would succeed", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed()})
		res := serve(h.c.App, http.MethodGet, "http://broker/health", "")
		assertEqual(t, res.Code, 200)
		assertJSON(t, res.Body.String(), `{"status":"ok"}`)
	})

	t.Run("503 when there is no login yet", func(t *testing.T) {
		h := newHarness(t, options{})
		res := serve(h.c.App, http.MethodGet, "http://broker/health", "")
		assertEqual(t, res.Code, 503)
	})
}

func TestUnknownRouteIs404(t *testing.T) {
	h := newHarness(t, options{seed: validSeed()})
	res := serve(h.c.App, http.MethodGet, "http://broker/nope", "")
	assertEqual(t, res.Code, 404)
}

func TestOptionalBearerGate(t *testing.T) {
	t.Run("gates /v1/session but leaves /health open", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed(), config: func(c *Config) { c.BearerToken = "s3cret" }})
		h.fake.on(routeNewSession, scripted{json: fixture("game-session.json")})
		body, _ := json.Marshal(map[string]string{"uuid": "x"})

		assertEqual(t, serve(h.c.App, http.MethodPost, brokerSession, string(body)).Code, 401) // no header
		assertEqual(t, serve(h.c.App, http.MethodPost, brokerSession, string(body), "Authorization", "Bearer wrong").Code, 401)
		assertEqual(t, serve(h.c.App, http.MethodGet, "http://broker/health", "").Code, 200) // health stays open

		res := serve(h.c.App, http.MethodPost, brokerSession, string(body), "Authorization", "Bearer s3cret")
		assertEqual(t, res.Code, 200)
	})
}
