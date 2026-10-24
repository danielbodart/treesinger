package broker

import (
	"context"
	"net/http"
	"time"

	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/gokit/environ"
	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/gokit/logx"
)

// Container is the one object graph. Production wires it once; each test wires
// a fresh one for an isolated graph. Leaves (Config, Clock, Logger, Sleep,
// Store, Transport, Retry) left nil get production defaults when Wire runs, so
// a test sets them to doubles *before* wiring. Everything downstream is derived
// by Wire, never set.
type Container struct {
	Config *Config
	Clock  clock.Clock
	Logger logx.Logger
	// The wait between device-flow polls, which gives up when the login is
	// abandoned: waited out on Clock unless a test replaces it.
	Sleep     func(ctx context.Context, d time.Duration) error
	Store     TokenStore
	Transport http.RoundTripper
	// The upstream retry policy, httpx.DefaultRetryPolicy unless a test
	// replaces it; its backoff is waited out on Clock when it names no clock
	// of its own.
	Retry *httpx.RetryPolicy

	OAuth    *OAuthClient
	Sessions *SessionClient
	App      http.Handler
	// Served only on the socket systemd passes as "admin"; see Admin.
	Admin http.Handler
}

// Wire fills in the leaves left unset and derives the rest from them.
func (c *Container) Wire() error {
	if c.Config == nil {
		config, err := ConfigFromEnv(environ.Current())
		if err != nil {
			return err
		}
		c.Config = &config
	}
	if c.Clock == nil {
		c.Clock = clock.System{}
	}
	if c.Logger == nil {
		c.Logger = logx.Console()
	}
	if c.Sleep == nil {
		waits := c.Clock
		c.Sleep = func(ctx context.Context, d time.Duration) error { return clock.Sleep(ctx, waits, d) }
	}
	if c.Store == nil {
		c.Store = NewFileTokenStore(c.Config.StorePath)
	}
	if c.Retry == nil {
		c.Retry = &httpx.DefaultRetryPolicy
	}
	if c.Transport == nil {
		c.Transport = http.DefaultTransport
	}
	retry := *c.Retry
	if retry.Clock == nil {
		retry.Clock = c.Clock
	}

	// Logged once around all of a request's retries, each attempt naming
	// Treesinger to upstream as treesinger/<version>.
	//
	// A retry resends a POST in full, the rotating refresh token's included: a
	// refresh that upstream processed but whose answer was lost fails on the
	// retry with a 401, and surfaces as re-auth needed (the documented
	// recovery), never a silent double-spend.
	client := &http.Client{Transport: httpx.Decorate(c.Transport,
		httpx.Logging(c.Logger, c.Clock),
		httpx.Retrying(retry),
		httpx.Identifying("treesinger/"+Version),
	)}
	c.OAuth = NewOAuthClient(OAuthDeps{Client: client, Clock: c.Clock, Store: c.Store, Logger: c.Logger, Config: c.Config, Sleep: c.Sleep})
	c.Sessions = NewSessionClient(SessionDeps{Client: client, Clock: c.Clock, OAuth: c.OAuth, Config: c.Config})
	c.App = &App{Sessions: c.Sessions, OAuth: c.OAuth, Logger: c.Logger}
	c.Admin = &Admin{OAuth: c.OAuth, Sessions: c.Sessions, Logger: c.Logger}
	// Optional hardening: gate /v1/session behind a bearer token when configured.
	if c.Config.BearerToken != "" {
		c.App = BearerGate(c.Config.BearerToken, c.App)
	}
	return nil
}
