package broker

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestDeviceFlow(t *testing.T) {
	ctx := context.Background()

	t.Run("polls through pending, then stores the refresh token on success", func(t *testing.T) {
		h := newHarness(t, options{})
		pending := scripted{status: 400, json: fixture("authorization-pending.json")}
		h.fake.on(routeDeviceAuth, scripted{json: fixture("device-auth.json")}).
			on(routeDeviceToken, pending, pending, scripted{json: fixture("token.json")})

		auth, err := h.c.OAuth.DeviceAuth(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.c.OAuth.PollToken(ctx, auth.DeviceCode, time.Duration(auth.Interval)*time.Second); err != nil {
			t.Fatal(err)
		}

		stored, _ := h.store.Read()
		assertEqual(t, stored.RefreshToken, "refresh-token-1")
		assertEqual(t, h.fake.countOf(routeDeviceToken), 3) // two pending + one success
	})

	t.Run("aborts on a hard error", func(t *testing.T) {
		h := newHarness(t, options{})
		h.fake.on(routeDeviceToken, scripted{status: 400, json: `{"error":"access_denied"}`})
		_, err := h.c.OAuth.PollToken(ctx, "dev-code", 5*time.Second)
		upstreamError(t, err)
	})
}

func TestEnsureAccessToken(t *testing.T) {
	ctx := context.Background()

	t.Run("cache hit: a comfortably-valid access token makes no network call", func(t *testing.T) {
		h := newHarness(t, options{seed: &StoredTokens{RefreshToken: "rt", AccessToken: "cached", AccessExpiresAt: epochMillis("2026-08-20T00:30:00Z")}})
		token, err := h.c.OAuth.EnsureAccessToken(ctx)
		assertEqual(t, token, "cached")
		assertEqual(t, err, nil)
		assertEqual(t, len(h.fake.recorded()), 0)
	})

	t.Run("refreshes when inside the skew window, and stores the rotated token before returning", func(t *testing.T) {
		// Access token expires in 2 min; skew is 5 min → stale.
		h := newHarness(t, options{seed: &StoredTokens{RefreshToken: "refresh-token-1", AccessToken: "old", AccessExpiresAt: epochMillis("2026-08-20T00:02:00Z")}})
		h.fake.on(routeRefresh, scripted{json: fixture("token-refreshed.json")})

		token, err := h.c.OAuth.EnsureAccessToken(ctx)

		assertEqual(t, err, nil)
		assertEqual(t, token, "access-token-2")
		stored, _ := h.store.Read()
		assertEqual(t, stored.RefreshToken, "refresh-token-2") // rotated + persisted
		assertEqual(t, stored.AccessToken, "access-token-2")
		assertEqual(t, h.fake.countOf(routeRefresh), 1)
	})

	t.Run("the refresh-or-not decision is pure over the clock", func(t *testing.T) {
		h := newHarness(t, options{seed: &StoredTokens{RefreshToken: "refresh-token-1", AccessToken: "old", AccessExpiresAt: epochMillis("2026-08-20T01:00:00Z")}})
		h.fake.on(routeRefresh, scripted{json: fixture("token-refreshed.json")})

		// 54 min in: 6 min of life left, clears the 5 min skew → cache hit.
		h.clock.Advance(54 * time.Minute)
		token, _ := h.c.OAuth.EnsureAccessToken(ctx)
		assertEqual(t, token, "old")
		assertEqual(t, h.fake.countOf(routeRefresh), 0)

		// 2 more min: 4 min left, inside skew → refresh.
		h.clock.Advance(2 * time.Minute)
		token, _ = h.c.OAuth.EnsureAccessToken(ctx)
		assertEqual(t, token, "access-token-2")
		assertEqual(t, h.fake.countOf(routeRefresh), 1)
	})

	t.Run("two concurrent stale calls share ONE refresh (no double-spend)", func(t *testing.T) {
		h := newHarness(t, options{seed: &StoredTokens{RefreshToken: "refresh-token-1", AccessExpiresAt: epochMillis("2026-08-20T00:02:00Z")}})
		h.fake.on(routeRefresh, scripted{json: fixture("token-refreshed.json")})

		tokens := make([]string, 2)
		var wg sync.WaitGroup
		for i := range tokens {
			wg.Go(func() { tokens[i], _ = h.c.OAuth.EnsureAccessToken(ctx) })
		}
		wg.Wait()

		assertEqual(t, tokens[0], "access-token-2")
		assertEqual(t, tokens[1], "access-token-2")
		assertEqual(t, h.fake.countOf(routeRefresh), 1) // the rotating token spent exactly once
	})

	t.Run("401 on refresh surfaces as re-auth-needed", func(t *testing.T) {
		h := newHarness(t, options{seed: &StoredTokens{RefreshToken: "stale", AccessExpiresAt: epochMillis("2026-08-20T00:02:00Z")}})
		h.fake.on(routeRefresh, scripted{status: 401, json: fixture("error-invalid-grant.json")})
		_, err := h.c.OAuth.EnsureAccessToken(ctx)
		assertEqual(t, upstreamError(t, err).Status, 401)
	})

	t.Run("no store at all is a 401, not a crash", func(t *testing.T) {
		h := newHarness(t, options{})
		_, err := h.c.OAuth.EnsureAccessToken(ctx)
		upstream := upstreamError(t, err)
		assertEqual(t, upstream.Status, 401)
		assertEqual(t, upstream.Code, "not_logged_in")
	})
}

func TestDeviceFlowSlowDownBacksOff(t *testing.T) {
	var slept []time.Duration
	h := newHarness(t, options{})
	h.c.OAuth.Sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	slow := scripted{status: 400, json: `{"error":"slow_down"}`}
	h.fake.on(routeDeviceToken, slow, slow, scripted{json: fixture("token.json")})

	if _, err := h.c.OAuth.PollToken(context.Background(), "dev", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, len(slept), 3)
	assertEqual(t, slept[0], 5*time.Second)
	assertEqual(t, slept[1], 10*time.Second) // each slow_down adds 5s, as RFC 8628 asks
	assertEqual(t, slept[2], 15*time.Second)
}
