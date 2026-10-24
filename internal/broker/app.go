package broker

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/danielbodart/gokit/logx"
)

// App is Treesinger's own HTTP surface, as an http.Handler.
//
//   - `POST /v1/session`: mint a fresh pair for the operator profile (optional
//     `{ "uuid": "..." }` body overrides which profile). What a booting server
//     calls.
//   - `GET /health`: 200 only when a mint would succeed (refresh token present
//     AND a valid access token obtainable). Backs compose `service_healthy`.
type App struct {
	Sessions *SessionClient
	OAuth    *OAuthClient
	Logger   logx.Logger
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/session":
		a.mintSession(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/health":
		a.health(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

// maxBodyBytes caps a mint request's body, which is at most a `{ "uuid" }`,
// so a client cannot make the broker buffer an arbitrary amount.
const maxBodyBytes = 64 << 10

func (a *App) mintSession(w http.ResponseWriter, r *http.Request) {
	uuid, err := profileFromBody(w, r)
	if tooLarge := (*http.MaxBytesError)(nil); errors.As(err, &tooLarge) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload_too_large"})
		return
	}
	if err == nil {
		var session GameSession
		if session, err = a.Sessions.NewSession(r.Context(), uuid); err == nil {
			writeJSON(w, http.StatusOK, session)
			return
		}
	}
	a.errorResponse(w, r, err)
}

func (a *App) health(w http.ResponseWriter, r *http.Request) {
	if _, err := a.OAuth.EnsureAccessToken(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unauthenticated"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// profileFromBody reads an optional `{ uuid }` from the body. An
// empty/whitespace body means "use the default profile"; a malformed body is
// the caller's error (400), not a broker fault (500).
func profileFromBody(w http.ResponseWriter, r *http.Request) (string, error) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return "", nil
	}
	var body struct {
		UUID string `json:"uuid"`
	}
	if json.Unmarshal(data, &body) != nil {
		return "", &UpstreamError{Status: 400, Code: "bad_request", Message: "body must be JSON with an optional string `uuid`"}
	}
	return body.UUID, nil
}

// errorResponse maps an upstream failure back to a client status; anything
// else is a 500. A mint abandoned because its client hung up is not logged as
// a failure.
func (a *App) errorResponse(w http.ResponseWriter, r *http.Request, err error) {
	var upstream *UpstreamError
	if errors.As(err, &upstream) {
		writeJSON(w, mapStatus(upstream.Status), map[string]string{"error": upstream.Code})
		return
	}
	if !cancelled(r, err) {
		a.Logger.Error("mint failed", err)
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
}

func mapStatus(status int) int {
	// Client errors (our 400 validation) and the auth/entitlement statuses pass
	// through unchanged; anything else upstream is a bad gateway to the fleet.
	switch status {
	case 400, 401, 403, 404:
		return status
	}
	return http.StatusBadGateway
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		status, data = http.StatusInternalServerError, []byte(`{"error":"internal"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(data)
}

// BearerGate is the optional bearer-token gate for `/v1/session`. Off unless a
// token is configured: the primary boundary is network isolation. `/health`
// stays open so compose health checks need no secret. The token is compared
// in constant time, so its prefix cannot be found by timing.
func BearerGate(token string, inner http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if r.URL.Path == "/v1/session" && subtle.ConstantTimeCompare(got, want) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		inner.ServeHTTP(w, r)
	})
}
