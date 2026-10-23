package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	profilesURL   = "https://account-data.hytale.com/my-account/get-profiles"
	sessionNewURL = "https://sessions.hytale.com/game-session/new"
)

// Cache profiles for an hour; they change rarely and each lookup costs a call.
const profilesTTL = time.Hour

type Profile struct {
	UUID     string `json:"uuid"`
	Username string `json:"username"`
}

type ProfilesResponse struct {
	Owner    string    `json:"owner"`
	Profiles []Profile `json:"profiles"`
}

// GameSession is the pair a Hytale server runs with. ExpiresAt is an ISO-8601
// instant.
type GameSession struct {
	SessionToken  string `json:"sessionToken"`
	IdentityToken string `json:"identityToken"`
	ExpiresAt     string `json:"expiresAt"`
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
	var profiles ProfilesResponse
	if err := s.call(ctx, http.MethodGet, profilesURL, token, nil, &profiles); err != nil {
		return ProfilesResponse{}, err
	}
	s.mu.Lock()
	s.profiles, s.profilesFetch = &profiles, now
	s.mu.Unlock()
	return profiles, nil
}

// resolveProfile is which profile a mint targets: an explicit override, else
// the configured OwnerUUID, else the account's first profile.
func (s *SessionClient) resolveProfile(ctx context.Context, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if s.Config.OwnerUUID != "" {
		return s.Config.OwnerUUID, nil
	}
	profiles, err := s.GetProfiles(ctx)
	if err != nil {
		return "", err
	}
	if len(profiles.Profiles) == 0 || profiles.Profiles[0].UUID == "" {
		return "", &UpstreamError{Status: 404, Code: "no_profile", Message: "account has no profiles"}
	}
	return profiles.Profiles[0].UUID, nil
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
	body, err := json.Marshal(map[string]string{"uuid": uuid})
	if err != nil {
		return session, err
	}
	return session, s.call(ctx, http.MethodPost, sessionNewURL, token, body, &session)
}

// call sends with a bearer token, and decodes a 2xx JSON response into `into`.
func (s *SessionClient) call(ctx context.Context, method, target, token string, body []byte, into any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := s.Client.Do(request)
	if err != nil {
		return err
	}
	return decode(response, into)
}
