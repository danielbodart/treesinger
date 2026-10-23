package broker

import (
	"context"
	"net/http"
	"time"
)

// Container is the one object graph. Production wires it once; each test wires
// a fresh one for an isolated graph. Leaves (Config, Clock, Logger, Sleep,
// Store, Transport, Retry) left nil get production defaults when Wire runs, so
// a test sets them to doubles *before* wiring. Everything downstream is derived
// by Wire, never set.
type Container struct {
	Config    *Config
	Clock     Clock
	Logger    Logger
	Sleep     func(ctx context.Context, d time.Duration) error
	Store     TokenStore
	Transport http.RoundTripper
	// The upstream retry policy; its backoff is a real timer unless a test
	// replaces it.
	Retry *RetryPolicy

	OAuth    *OAuthClient
	Sessions *SessionClient
	App      http.Handler
	// Served only on the socket systemd passes as "admin"; see Admin.
	Admin http.Handler
}

// Wire fills in the leaves left unset and derives the rest from them.
func (c *Container) Wire() error {
	if c.Config == nil {
		config, err := ConfigFromEnv(Environ())
		if err != nil {
			return err
		}
		c.Config = &config
	}
	if c.Clock == nil {
		c.Clock = SystemClock{}
	}
	if c.Logger == nil {
		c.Logger = ConsoleLogger()
	}
	if c.Sleep == nil {
		c.Sleep = delay
	}
	if c.Store == nil {
		c.Store = NewFileTokenStore(c.Config.StorePath)
	}
	if c.Retry == nil {
		c.Retry = &DefaultRetryPolicy
	}
	if c.Transport == nil {
		c.Transport = http.DefaultTransport
	}

	// Same-shape wrappers: name Treesinger to upstream, retry transient upstream
	// failures, then log.
	client := &http.Client{Transport: Logging(c.Logger)(Retrying(*c.Retry)(Identifying(Version)(c.Transport)))}
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
