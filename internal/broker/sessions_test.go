package broker

import (
	"context"
	"testing"
	"time"
)

func TestMint(t *testing.T) {
	ctx := context.Background()

	t.Run("profiles → new session → the pair, minted for the first profile by default", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed()})
		h.fake.on(routeProfiles, scripted{json: fixture("profiles.json")}).on(routeNewSession, scripted{json: fixture("game-session.json")})

		session, err := h.c.Sessions.NewSession(ctx, "")

		assertEqual(t, err, nil)
		assertEqual(t, session.SessionToken, "session-jwt-1")
		assertEqual(t, session.IdentityToken, "identity-jwt-1")
		assertEqual(t, session.ExpiresAt, "2026-08-20T01:00:00Z")
		mint, _ := h.fake.find("/game-session/new")
		assertJSON(t, mint.body, `{"uuid":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}`)
		assertEqual(t, mint.authorization, "Bearer access-token-1")
	})

	t.Run("configured ownerUuid wins and skips the profiles lookup", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed(), config: func(c *Config) { c.OwnerUUID = "dddddddd-dddd-dddd-dddd-dddddddddddd" }})
		h.fake.on(routeNewSession, scripted{json: fixture("game-session.json")})

		h.c.Sessions.NewSession(ctx, "")

		assertEqual(t, h.fake.countOf(routeProfiles), 0)
		mint, _ := h.fake.find("/game-session/new")
		assertJSON(t, mint.body, `{"uuid":"dddddddd-dddd-dddd-dddd-dddddddddddd"}`)
	})

	t.Run("a per-call uuid override beats both config and the default", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed(), config: func(c *Config) { c.OwnerUUID = "dddddddd-dddd-dddd-dddd-dddddddddddd" }})
		h.fake.on(routeNewSession, scripted{json: fixture("game-session.json")})

		h.c.Sessions.NewSession(ctx, "cccccccc-cccc-cccc-cccc-cccccccccccc")

		mint, _ := h.fake.find("/game-session/new")
		assertJSON(t, mint.body, `{"uuid":"cccccccc-cccc-cccc-cccc-cccccccccccc"}`)
	})

	t.Run("403 over the session limit surfaces as 403", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed(), config: func(c *Config) { c.OwnerUUID = "x" }})
		h.fake.on(routeNewSession, scripted{status: 403, json: fixture("error-session-limit.json")})
		_, err := h.c.Sessions.NewSession(ctx, "")
		upstream := upstreamError(t, err)
		assertEqual(t, upstream.Status, 403)
		assertEqual(t, upstream.Code, "session_limit_exceeded")
	})
}

func TestProfilesCache(t *testing.T) {
	t.Run("cached within the TTL, refetched after it", func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t, options{seed: validSeed()})
		h.fake.on(routeProfiles, scripted{json: fixture("profiles.json")})

		h.c.Sessions.GetProfiles(ctx)
		h.c.Sessions.GetProfiles(ctx)
		assertEqual(t, h.fake.countOf(routeProfiles), 1) // second served from cache

		h.clock.Advance(61 * time.Minute) // past the 1h TTL
		h.c.Sessions.GetProfiles(ctx)
		assertEqual(t, h.fake.countOf(routeProfiles), 2)
	})
}
