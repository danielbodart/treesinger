package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	profilesURL       = "https://account-data.hytale.com/my-account/get-profiles"
	sessionNewURL     = "https://sessions.hytale.com/game-session/new"
	sessionRefreshURL = "https://sessions.hytale.com/game-session/refresh"
	sessionURL        = "https://sessions.hytale.com/game-session"
)

// Cache profiles for an hour; they change rarely and each lookup costs a call.
const profilesTTL = time.Hour

type Profile struct {
	UUID     string `json:"uuid"`
	Username string `json:"username"`
}

// ProfilesResponse is get-profiles' answer. Upstream's JSON is read the way the
// TypeScript's unchecked cast reads it: keys matched exactly, and the profiles
// kept as sent, since what a mint posts depends on them exactly.
type ProfilesResponse struct {
	Owner    string    `json:"owner"`
	Profiles []Profile `json:"profiles"`

	// The profiles as sent, or nil when there was no `profiles` array.
	listed []json.RawMessage
}

func (p *ProfilesResponse) UnmarshalJSON(data []byte) error {
	object, _ := jsonObject(data)
	*p = ProfilesResponse{Owner: jsonText(object["owner"])}
	if json.Unmarshal(object["profiles"], &p.listed) != nil || p.listed == nil {
		p.listed = nil
		return nil
	}
	p.Profiles = make([]Profile, len(p.listed))
	for i, raw := range p.listed {
		fields, _ := jsonObject(raw)
		p.Profiles[i] = Profile{UUID: jsonText(fields["uuid"]), Username: jsonText(fields["username"])}
	}
	return nil
}

// GameSession is the pair a Hytale server runs with. ExpiresAt is an ISO-8601
// instant.
//
// One read from upstream is passed on to the client exactly as upstream sent
// it, as the TypeScript (which never looks inside it) does: fields beyond these
// three, values of other types, and even a `null`. The typed fields are what
// could be read of it, by their exact keys, for callers in Go.
type GameSession struct {
	SessionToken  string `json:"sessionToken"`
	IdentityToken string `json:"identityToken"`
	ExpiresAt     string `json:"expiresAt"`

	raw json.RawMessage
}

func (s *GameSession) UnmarshalJSON(data []byte) error {
	object, _ := jsonObject(data)
	*s = GameSession{
		SessionToken:  jsonText(object["sessionToken"]),
		IdentityToken: jsonText(object["identityToken"]),
		ExpiresAt:     jsonText(object["expiresAt"]),
		raw:           append(json.RawMessage(nil), data...),
	}
	return nil
}

func (s GameSession) MarshalJSON() ([]byte, error) {
	if s.raw != nil {
		return s.raw, nil
	}
	type fields GameSession
	return json.Marshal(fields(s))
}

type SessionDeps struct {
	Client *http.Client
	Clock  Clock
	OAuth  *OAuthClient
	Config *Config
}

type SessionClient struct {
	SessionDeps

	mu            sync.Mutex
	profiles      *ProfilesResponse
	profilesFetch time.Time
}

func NewSessionClient(deps SessionDeps) *SessionClient { return &SessionClient{SessionDeps: deps} }

// GetProfiles is §3: the operator's account profiles, cached with a TTL over
// the clock.
func (s *SessionClient) GetProfiles(ctx context.Context) (ProfilesResponse, error) {
	now := s.Clock.Now()
	s.mu.Lock()
	cached, at := s.profiles, s.profilesFetch
	s.mu.Unlock()
	if cached != nil && now.Sub(at) < profilesTTL {
		return *cached, nil
	}
	token, err := s.OAuth.EnsureAccessToken(ctx)
	if err != nil {
		return ProfilesResponse{}, err
	}
	var value ProfilesResponse
	if err := s.call(ctx, http.MethodGet, profilesURL, token, nil, &value); err != nil {
		return ProfilesResponse{}, err
	}
	s.mu.Lock()
	s.profiles, s.profilesFetch = &value, now
	s.mu.Unlock()
	return value, nil
}

// ResolveProfile is which profile a mint targets: an explicit override, else
// the configured OwnerUUID, else the account's first profile.
func (s *SessionClient) ResolveProfile(ctx context.Context, override string) (string, error) {
	uuid, err := s.resolveProfile(ctx, override)
	return jsonText(uuid), err
}

// resolveProfile is ResolveProfile's answer as the JSON value it posts: nil
// when the first profile has no uuid (which the TypeScript's JSON.stringify
// then leaves out of the body), or whatever value upstream gave it.
func (s *SessionClient) resolveProfile(ctx context.Context, override string) (json.RawMessage, error) {
	if override != "" {
		return marshal(override)
	}
	if s.Config.OwnerUUID != "" {
		return marshal(s.Config.OwnerUUID)
	}
	profiles, err := s.GetProfiles(ctx)
	if err != nil {
		return nil, err
	}
	// The TypeScript destructures `profiles` and indexes it, which throws when
	// there is no list at all: a broker fault, not "no profiles".
	if profiles.listed == nil {
		return nil, errors.New("get-profiles answered without a profiles list")
	}
	if len(profiles.listed) == 0 || isFalsy(profiles.listed[0]) {
		return nil, &UpstreamError{Status: 404, Code: "no_profile", Message: "account has no profiles"}
	}
	first, _ := jsonObject(profiles.listed[0])
	if uuid := first["uuid"]; uuid != nil {
		return uuid, nil
	}
	return nil, nil
}

// NewSession is §4: mint a fresh session/identity pair for a profile ("" for
// the default one).
func (s *SessionClient) NewSession(ctx context.Context, profileUUID string) (GameSession, error) {
	var session GameSession
	uuid, err := s.resolveProfile(ctx, profileUUID)
	if err != nil {
		return session, err
	}
	token, err := s.OAuth.EnsureAccessToken(ctx)
	if err != nil {
		return session, err
	}
	fields := map[string]json.RawMessage{}
	if uuid != nil {
		fields["uuid"] = uuid
	}
	body, err := marshal(fields)
	if err != nil {
		return session, err
	}
	return session, s.call(ctx, http.MethodPost, sessionNewURL, token, body, &session)
}

// RefreshSession is §5: refresh a session with its own session token.
// Treesinger does not do this for running servers (they self-refresh); exposed
// for completeness of the client surface.
func (s *SessionClient) RefreshSession(ctx context.Context, sessionToken string) (GameSession, error) {
	var session GameSession
	return session, s.call(ctx, http.MethodPost, sessionRefreshURL, sessionToken, nil, &session)
}

// TerminateSession terminates a session.
func (s *SessionClient) TerminateSession(ctx context.Context, sessionToken string) error {
	response, err := s.send(ctx, http.MethodDelete, sessionURL, sessionToken, nil)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if !succeeded(response) {
		return &UpstreamError{Status: response.StatusCode, Code: "terminate_failed", Message: fmt.Sprintf("terminate failed: %d", response.StatusCode)}
	}
	return nil
}

// call sends with a bearer token, and decodes a 2xx JSON response into `into`.
func (s *SessionClient) call(ctx context.Context, method, target, token string, body []byte, into any) error {
	response, err := s.send(ctx, method, target, token, body)
	if err != nil {
		return err
	}
	return ok(response, into)
}

func (s *SessionClient) send(ctx context.Context, method, target, token string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return s.Client.Do(request)
}
