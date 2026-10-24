package broker

import (
	"encoding/json"
	"net/http"
	"sync/atomic"

	"github.com/danielbodart/gokit/logx"
)

// Admin is the broker's privileged HTTP surface, as an http.Handler. It is
// served only on the socket systemd passes under the name "admin"
// (treesinger-admin.socket, root only), never on a socket or port the broker
// binds itself: the socket's mode is its only access check.
//
//   - `POST /v1/login`: run the device flow in the broker, through the same
//     OAuthClient that refreshes, so the login and a refresh never write over
//     each other, and the very next mint uses the new account without a
//     restart. Its answer is newline-delimited JSON over one response: first
//     `{ verification_uri_complete, verification_uri, user_code }` for the
//     operator, then `{ "ok": true }` or `{ "error": "..." }` once it is
//     decided. One login at a time; another is a 409 while it runs.
type Admin struct {
	OAuth    *OAuthClient
	Sessions *SessionClient
	Logger   logx.Logger

	loggingIn atomic.Bool
}

func (a *Admin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == "/v1/login" {
		a.login(w, r)
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
}

// loginPrompt is the first event of a login: what the operator needs to
// approve it.
type loginPrompt struct {
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	VerificationURI         string `json:"verification_uri,omitempty"`
	UserCode                string `json:"user_code,omitempty"`
}

// loginEvent is any event of a login, as the client reads it.
type loginEvent struct {
	loginPrompt
	OK    bool   `json:"ok,omitempty"`
	Error string `json:"error,omitempty"`
}

func (a *Admin) login(w http.ResponseWriter, r *http.Request) {
	if !a.loggingIn.CompareAndSwap(false, true) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "login_in_progress"})
		return
	}
	defer a.loggingIn.Store(false)

	// Committed to a 200 from here: every outcome, failure included, is an
	// event, since the status is long gone by the time the outcome is known.
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	send := func(event any) {
		data, _ := json.Marshal(event)
		w.Write(append(data, '\n'))
		// Each line reaches the operator as it happens, not when the response
		// ends. A writer that cannot flush (one in memory) has nothing to wait
		// for.
		http.NewResponseController(w).Flush()
	}

	// The request's context: an operator who gives up (^C) abandons the
	// login, which frees the next one at once.
	err := a.OAuth.Login(r.Context(), func(auth DeviceAuth) {
		send(loginPrompt{auth.VerificationURIComplete, auth.VerificationURI, auth.UserCode})
	})
	if err != nil {
		if !cancelled(r, err) {
			a.Logger.Error("login failed", err)
		}
		send(loginEvent{Error: err.Error()})
		return
	}
	// The profiles were the old login's, perhaps another account's.
	a.Sessions.ForgetProfiles()
	a.Logger.Log("logged in; refresh token stored")
	send(loginEvent{OK: true})
}
