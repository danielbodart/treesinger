package broker

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// OAuth 2.0 client for `client_id=hytale-server`, per the Server Provider guide.
const (
	clientID        = "hytale-server"
	scope           = "openid offline auth:server"
	deviceAuthURL   = "https://oauth.accounts.hytale.com/oauth2/device/auth"
	tokenURL        = "https://oauth.accounts.hytale.com/oauth2/token"
	deviceCodeGrant = "urn:ietf:params:oauth:grant-type:device_code"
)

type DeviceAuth struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	// Seconds to wait between polls.
	Interval int `json:"interval"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

type OAuthDeps struct {
	Client *http.Client
	Clock  Clock
	Store  TokenStore
	Logger Logger
	Config *Config
	// Between-poll wait during the device flow, which gives up when the login
	// is abandoned. Instant in tests.
	Sleep func(ctx context.Context, d time.Duration) error
}

type OAuthClient struct {
	OAuthDeps

	// The single-flight latch: concurrent EnsureAccessToken callers that all
	// find the cached token stale share ONE refresh instead of each spending
	// the rotating refresh token. This, not a lock around the refresh, is what
	// makes double-spend structurally impossible.
	mu       sync.Mutex
	inflight *flight

	// Serialises the store's two writers, a refresh and a login, from the
	// refresh's read to its write. Without it a refresh already in flight when a
	// login completes would land after it, and put back the tokens the login
	// replaced (perhaps another account's).
	writing sync.Mutex
}

// flight is one refresh, awaited by everyone who asked while it ran.
type flight struct {
	done  chan struct{}
	token string
	err   error
}

func NewOAuthClient(deps OAuthDeps) *OAuthClient { return &OAuthClient{OAuthDeps: deps} }

// DeviceAuth is §1: start the device flow. Show the user VerificationURI +
// UserCode.
func (o *OAuthClient) DeviceAuth(ctx context.Context) (DeviceAuth, error) {
	var auth DeviceAuth
	response, err := o.post(ctx, deviceAuthURL, url.Values{"client_id": {clientID}, "scope": {scope}})
	if err != nil {
		return auth, err
	}
	return auth, decode(response, &auth)
}

// PollToken is §2: poll the token endpoint until the user approves, then
// persist the resulting tokens (this is what `login` writes).
// `authorization_pending` continues the poll and `slow_down` continues it 5s
// further apart, as RFC 8628 asks; any other error aborts.
func (o *OAuthClient) PollToken(ctx context.Context, deviceCode string, interval time.Duration) (StoredTokens, error) {
	form := url.Values{"client_id": {clientID}, "grant_type": {deviceCodeGrant}, "device_code": {deviceCode}}
	for {
		if err := o.Sleep(ctx, interval); err != nil {
			return StoredTokens{}, err
		}
		response, err := o.post(ctx, tokenURL, form)
		if err != nil {
			return StoredTokens{}, err
		}
		if succeeded(response) {
			var token tokenResponse
			if err := decode(response, &token); err != nil {
				return StoredTokens{}, err
			}
			o.writing.Lock()
			defer o.writing.Unlock()
			return o.persist(token)
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			return StoredTokens{}, err
		}
		switch code := errorCode(data); code {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		default:
			return StoredTokens{}, &UpstreamError{Status: response.StatusCode, Code: code, Message: "device token poll failed: " + code}
		}
	}
}

// Login is the whole device flow, §1–2, for both ways of logging in (the
// broker's admin socket, and a local `treesinger login`): start it, hand the
// code to prompt for the operator, then poll until they approve and the
// refresh token is stored.
func (o *OAuthClient) Login(ctx context.Context, prompt func(DeviceAuth)) error {
	auth, err := o.DeviceAuth(ctx)
	if err != nil {
		return err
	}
	prompt(auth)
	_, err = o.PollToken(ctx, auth.DeviceCode, time.Duration(auth.Interval)*time.Second)
	return err
}

// EnsureAccessToken is a valid OAuth access token, refreshing on demand. Fast
// path: a cached token with more than the skew window of life left is returned
// without any network call. Otherwise a single shared refresh runs (see
// inflight).
func (o *OAuthClient) EnsureAccessToken(ctx context.Context) (string, error) {
	stored, err := o.Store.Read()
	if err != nil {
		return "", err
	}
	if stored == nil {
		return "", &UpstreamError{Status: 401, Code: "not_logged_in", Message: "no refresh token in store — run `treesinger login`"}
	}
	if o.isFresh(stored) {
		return stored.AccessToken, nil
	}

	o.mu.Lock()
	current := o.inflight
	if current == nil {
		current = &flight{done: make(chan struct{})}
		o.inflight = current
		// Detached from this caller's cancellation: the others are waiting on
		// it too, and a refresh abandoned halfway may already have spent the
		// token.
		go func() {
			current.token, current.err = o.refresh(context.WithoutCancel(ctx))
			o.mu.Lock()
			o.inflight = nil
			o.mu.Unlock()
			close(current.done)
		}()
	}
	o.mu.Unlock()

	select {
	case <-current.done:
		return current.token, current.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// refresh is §6: spend the refresh token for a fresh access+refresh pair. The
// rotated refresh token is written to the store BEFORE the new access token is
// returned, so a crash mid-flight can never lose the only live credential.
// Returns the new access token.
func (o *OAuthClient) refresh(ctx context.Context) (string, error) {
	o.writing.Lock()
	defer o.writing.Unlock()
	stored, err := o.Store.Read()
	if err != nil {
		return "", err
	}
	if stored == nil || stored.RefreshToken == "" {
		return "", &UpstreamError{Status: 401, Code: "not_logged_in", Message: "no refresh token in store"}
	}
	// Double-check under the single-flight: a refresh that completed between
	// the caller's cache read and here has already rotated + stored a fresh
	// token; return it rather than spending the (now-invalidated) old one.
	if o.isFresh(stored) {
		return stored.AccessToken, nil
	}
	o.Logger.Log("refreshing OAuth access token")
	response, err := o.post(ctx, tokenURL, url.Values{"client_id": {clientID}, "grant_type": {"refresh_token"}, "refresh_token": {stored.RefreshToken}})
	if err != nil {
		return "", err
	}
	if response.StatusCode == http.StatusUnauthorized {
		response.Body.Close()
		return "", &UpstreamError{Status: 401, Code: "invalid_grant", Message: "refresh token rejected — re-auth needed"}
	}
	var token tokenResponse
	if err := decode(response, &token); err != nil {
		return "", err
	}
	// Some servers omit a rotated token; keep the existing one if so.
	if token.RefreshToken == "" {
		token.RefreshToken = stored.RefreshToken
	}
	persisted, err := o.persist(token)
	return persisted.AccessToken, err
}

// persist computes expiry from expires_in and writes the store atomically.
func (o *OAuthClient) persist(token tokenResponse) (StoredTokens, error) {
	tokens := StoredTokens{
		RefreshToken:    token.RefreshToken,
		AccessToken:     token.AccessToken,
		AccessExpiresAt: o.Clock.Now().Add(time.Duration(token.ExpiresIn) * time.Second).UnixMilli(),
	}
	return tokens, o.Store.Write(tokens)
}

// isFresh is pure over clock + stored expiry: is there a cached token, and
// does it clear the skew window?
func (o *OAuthClient) isFresh(stored *StoredTokens) bool {
	return stored.AccessToken != "" &&
		o.Clock.Now().Add(o.Config.Skew).UnixMilli() < stored.AccessExpiresAt
}

// post sends a form-encoded POST.
func (o *OAuthClient) post(ctx context.Context, target string, form url.Values) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return o.Client.Do(request)
}
