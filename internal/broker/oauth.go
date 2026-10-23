package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
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
	DeviceCode              string  `json:"device_code"`
	UserCode                string  `json:"user_code"`
	VerificationURI         string  `json:"verification_uri"`
	VerificationURIComplete string  `json:"verification_uri_complete"`
	ExpiresIn               float64 `json:"expires_in"`
	Interval                float64 `json:"interval"`
}

type tokenResponse struct {
	AccessToken  string  `json:"access_token"`
	TokenType    string  `json:"token_type"`
	ExpiresIn    float64 `json:"expires_in"`
	RefreshToken string  `json:"refresh_token"`
	Scope        string  `json:"scope"`
}

// UnmarshalJSON reads a token response as the TypeScript's unchecked cast
// does, never failing on a field's type: a refresh that upstream has already
// spent must always get its rotated token persisted. Keys match exactly;
// `expires_in` coerces as `expires_in * 1000` does (a numeric string counts,
// a missing one is NaN), and a token that is not a string is String() of it.
// Only a body that is not an object at all, on which the TypeScript throws
// too, is an error.
func (t *tokenResponse) UnmarshalJSON(data []byte) error {
	object, isObject := jsonObject(data)
	if !isObject {
		return errors.New("token response is not a JSON object")
	}
	*t = tokenResponse{
		AccessToken:  jsonText(object["access_token"]),
		TokenType:    jsonText(object["token_type"]),
		ExpiresIn:    jsonNumber(object["expires_in"]),
		RefreshToken: jsonText(object["refresh_token"]),
		Scope:        jsonText(object["scope"]),
	}
	return nil
}

type OAuthDeps struct {
	Client *http.Client
	Clock  Clock
	Store  TokenStore
	Logger Logger
	Config *Config
	// Between-poll wait during the device flow. Instant in tests.
	Sleep func(time.Duration)
}

type OAuthClient struct {
	OAuthDeps

	// The single-flight latch: concurrent EnsureAccessToken callers that all
	// find the cached token stale share ONE refresh instead of each spending
	// the rotating refresh token. This, not a lock around the refresh, is what
	// makes double-spend structurally impossible.
	mu       sync.Mutex
	inflight *flight
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
	response, err := o.post(ctx, tokenForm(deviceAuthURL, "client_id", clientID, "scope", scope))
	if err != nil {
		return auth, err
	}
	return auth, ok(response, &auth)
}

// PollToken is §2: poll the token endpoint until the user approves, then
// persist the resulting tokens (this is what `login` writes).
// `authorization_pending` and `slow_down` continue the poll; any other error
// aborts.
//
// Deviation, kept: with no `interval` in the device-auth response the
// TypeScript polls every NaN seconds (a 1ms timer) forever, slow_down
// included, since NaN + 5 is NaN. Here a missing interval is 0, and each
// slow_down adds 5s to it, so polling backs off as RFC 8628 asks.
func (o *OAuthClient) PollToken(ctx context.Context, deviceCode string, interval time.Duration) (StoredTokens, error) {
	for {
		o.Sleep(interval)
		response, err := o.post(ctx, tokenForm(tokenURL, "client_id", clientID, "grant_type", deviceCodeGrant, "device_code", deviceCode))
		if err != nil {
			return StoredTokens{}, err
		}
		if succeeded(response) {
			var token tokenResponse
			if err := ok(response, &token); err != nil {
				return StoredTokens{}, err
			}
			return o.persist(token)
		}
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		value, found := errorField(data)
		code, isString := jsonString(value)
		switch {
		case isString && code == "authorization_pending":
			continue
		case isString && code == "slow_down":
			interval += 5 * time.Second
			continue
		case found:
			code = jsonText(value)
			return StoredTokens{}, &UpstreamError{Status: response.StatusCode, Code: code, Value: value, Message: "device token poll failed: " + code}
		default:
			return StoredTokens{}, &UpstreamError{Status: response.StatusCode, Code: "unknown", Message: fmt.Sprintf("device token poll failed: %d", response.StatusCode)}
		}
	}
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
	if stored.AccessToken != "" && o.isFresh(stored) {
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
	if stored.AccessToken != "" && o.isFresh(stored) {
		return stored.AccessToken, nil
	}
	o.Logger.Log("refreshing OAuth access token")
	response, err := o.post(ctx, tokenForm(tokenURL, "client_id", clientID, "grant_type", "refresh_token", "refresh_token", stored.RefreshToken))
	if err != nil {
		return "", err
	}
	if response.StatusCode == http.StatusUnauthorized {
		response.Body.Close()
		return "", &UpstreamError{Status: 401, Code: "invalid_grant", Message: "refresh token rejected — re-auth needed"}
	}
	var token tokenResponse
	if err := ok(response, &token); err != nil {
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
// An expiry that is not a finite number (which JSON.stringify writes as null)
// is stored as none, so the token is refreshed on next use, as there.
func (o *OAuthClient) persist(token tokenResponse) (StoredTokens, error) {
	tokens := StoredTokens{RefreshToken: token.RefreshToken, AccessToken: token.AccessToken}
	if expiresAt := float64(o.Clock.Now().UnixMilli()) + token.ExpiresIn*1000; !math.IsInf(expiresAt, 0) {
		tokens.AccessExpiresAt = millisAt(expiresAt)
	}
	return tokens, o.Store.Write(tokens)
}

// isFresh is pure over clock + stored expiry: does the cached token clear the
// skew window?
func (o *OAuthClient) isFresh(stored *StoredTokens) bool {
	return stored.AccessExpiresAt != nil &&
		o.Clock.Now().UnixMilli() < *stored.AccessExpiresAt-o.Config.Skew.Milliseconds()
}

func (o *OAuthClient) post(ctx context.Context, form *http.Request) (*http.Response, error) {
	return o.Client.Do(form.WithContext(ctx))
}

// tokenForm builds a form-encoded POST from name/value pairs, kept in order.
func tokenForm(target string, pairs ...string) *http.Request {
	fields := make([]string, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		fields = append(fields, url.QueryEscape(pairs[i])+"="+url.QueryEscape(pairs[i+1]))
	}
	request, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(strings.Join(fields, "&")))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return request
}
