package broker

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
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
	Logger   Logger
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := requestPath(r)
	switch {
	case r.Method == http.MethodPost && path == "/v1/session":
		a.mintSession(w, r)
	case r.Method == http.MethodGet && path == "/health":
		a.health(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

// requestPath is the path a request is routed by, normalised as the WHATWG
// URL parser that Bun's `new URL(request.url).pathname` uses normalises it: a
// backslash is a slash, and "." and ".." segments (also spelled %2e) are
// resolved, so `/v1/./session`, `/x/../v1/session` and `/v1\session` are all
// `/v1/session`. Empty segments stay, as they do there. Routing and the bearer
// gate both use it, so they always agree on which route a request is for.
func requestPath(r *http.Request) string {
	// The target as sent, where there is one: r.URL has already decoded %5C
	// into a backslash, which the WHATWG parser leaves encoded.
	raw := r.URL.EscapedPath()
	if strings.HasPrefix(r.RequestURI, "/") {
		raw, _, _ = strings.Cut(r.RequestURI, "?")
	}
	segments := strings.Split(strings.ReplaceAll(raw, `\`, "/"), "/")[1:]
	resolved := make([]string, 0, len(segments))
	for i, segment := range segments {
		last := i == len(segments)-1
		switch strings.ToLower(segment) {
		case "..", ".%2e", "%2e.", "%2e%2e":
			if len(resolved) > 0 {
				resolved = resolved[:len(resolved)-1]
			}
			if last {
				resolved = append(resolved, "")
			}
		case ".", "%2e":
			if last {
				resolved = append(resolved, "")
			}
		default:
			resolved = append(resolved, segment)
		}
	}
	return "/" + strings.Join(resolved, "/")
}

// maxBodyBytes caps a mint request's body, which is at most a `{ "uuid" }`.
// Bun refuses a body over its limit with 413 rather than buffering it, and so
// does this, at a limit sized for what the route takes.
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
// the caller's error (400), not a broker fault (500). The body is read as
// `request.text()` then `.trim()` read it: a leading byte-order mark dropped,
// and JavaScript's whitespace (U+FEFF among it) trimmed.
func profileFromBody(w http.ResponseWriter, r *http.Request) (string, error) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return "", err
	}
	text := strings.TrimPrefix(string(data), "\ufeff")
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "\ufffd")
	}
	text = jsTrim(text)
	if text == "" {
		return "", nil
	}
	badRequest := &UpstreamError{Status: 400, Code: "bad_request", Message: "body must be JSON with an optional string `uuid`"}
	var body any
	if json.Unmarshal([]byte(text), &body) != nil {
		return "", badRequest
	}
	object, _ := body.(map[string]any)
	switch uuid := object["uuid"].(type) {
	case nil:
		return "", nil
	case string:
		return uuid, nil
	default:
		return "", badRequest
	}
}

// errorResponse maps an upstream failure back to a client status; anything
// else is a 500. A mint abandoned because its client hung up is not logged as
// a failure: the TypeScript never cancels one, so never logs one.
func (a *App) errorResponse(w http.ResponseWriter, r *http.Request, err error) {
	var upstream *UpstreamError
	if errors.As(err, &upstream) {
		var code any = upstream.Code
		if upstream.Value != nil {
			code = upstream.Value
		}
		writeJSON(w, mapStatus(upstream.Status), map[string]any{"error": code})
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
	data, err := marshal(body)
	if err != nil {
		status, data = http.StatusInternalServerError, []byte(`{"error":"internal"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(data)
}

// BearerGate is the optional bearer-token gate for `/v1/session`. Off unless a
// token is configured: the primary boundary is network isolation. `/health`
// stays open so compose health checks need no secret.
func BearerGate(token string, inner http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(strings.Join(r.Header.Values("Authorization"), ", "))
		if requestPath(r) == "/v1/session" && subtle.ConstantTimeCompare(got, want) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		inner.ServeHTTP(w, r)
	})
}
