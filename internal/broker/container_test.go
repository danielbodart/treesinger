package broker

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/gokit/logx/logtest"
)

// The decorators themselves are gokit's, and tested there; these are how the
// container puts them together.
func TestTheUpstreamClient(t *testing.T) {
	t.Run("logs once around all the retries", func(t *testing.T) {
		logger := &logtest.Logger{}
		h := newHarness(t, options{seed: validSeed()})
		h.c.Logger = logger
		h.c.Wire()
		h.fake.on(routeProfiles, scripted{status: 503, json: `{}`}, scripted{json: fixture("profiles.json")})

		h.c.Sessions.GetProfiles(context.Background())

		assertEqual(t, h.fake.countOf(routeProfiles), 2)
		entries := logger.Entries()
		assertEqual(t, len(entries), 1) // the 503 was retried inside the one logged call
		// Timed on the stopped clock, so it took no time at all.
		assertEqual(t, entries[0].String(), "log: GET https://account-data.hytale.com/my-account/get-profiles → 200 (0ms)")
		assertEqual(t, len(h.waits.Waits()), 1)
		assertEqual(t, h.waits.Waits()[0], 200*time.Millisecond) // the default policy's first backoff
	})

	t.Run("names Treesinger to upstream, on every attempt", func(t *testing.T) {
		var agents []string
		h := newHarness(t, options{seed: validSeed()})
		h.c.Transport = httpx.RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			agents = append(agents, r.Header.Get("User-Agent"))
			return httpx.HandlerTransport(h.fake).RoundTrip(r)
		})
		h.c.Wire()
		h.fake.on(routeProfiles, scripted{status: 503, json: `{}`}, scripted{json: fixture("profiles.json")})

		h.c.Sessions.GetProfiles(context.Background())

		assertEqual(t, len(agents), 2)
		for _, agent := range agents {
			assertEqual(t, agent, "treesinger/"+Version)
		}
	})
}
