package broker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/danielbodart/gokit/httpx"
)

// The operator's view of an approved login, after the line saying how.
const shownPrompt = `
To authorise Treesinger, visit:

  https://accounts.hytale.com/device?user_code=ABCD-1234

or go to https://accounts.hytale.com/device and enter code: ABCD-1234

Waiting for approval…
`

// noLocalLogin fails the test if Login falls back to logging in here.
func noLocalLogin(t *testing.T) func() (*Container, error) {
	return func() (*Container, error) {
		t.Error("fell back to a local login")
		return nil, errors.New("no local login")
	}
}

func TestLoginThroughTheBroker(t *testing.T) {
	ctx := context.Background()

	t.Run("shows the broker's prompt, then that it worked", func(t *testing.T) {
		h := newHarness(t, options{})
		scriptLogin(h)
		socket := unixServer(t, h.c.Admin)
		var out bytes.Buffer

		err := Login(ctx, nil, socket, &out, noLocalLogin(t))

		assertEqual(t, err, nil)
		assertEqual(t, out.String(), "Logging in through the broker at "+socket+"."+"\n"+shownPrompt+
			"✓ Logged in. Refresh token stored, and the broker is using it now.\n")
		stored, _ := h.store.Read()
		assertEqual(t, stored.RefreshToken, "refresh-token-1")
	})

	t.Run("--socket names the socket", func(t *testing.T) {
		h := newHarness(t, options{})
		scriptLogin(h)
		socket := unixServer(t, h.c.Admin)
		var out bytes.Buffer
		assertEqual(t, Login(ctx, []string{"--socket", socket}, "/nonexistent/admin.sock", &out, noLocalLogin(t)), nil)
	})

	t.Run("a login the broker reports failed is an error", func(t *testing.T) {
		h := newHarness(t, options{})
		h.fake.on(routeDeviceAuth, scripted{json: fixture("device-auth.json")}).
			on(routeDeviceToken, scripted{status: 400, json: `{"error":"access_denied"}`})

		err := remoteLogin(ctx, &http.Client{Transport: httpx.HandlerTransport(h.c.Admin)}, "admin.sock", io.Discard)

		assertErrorContains(t, err, "login failed: device token poll failed: access_denied")
	})

	t.Run("a login already running is an error", func(t *testing.T) {
		busy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "login_in_progress"})
		})
		err := remoteLogin(ctx, &http.Client{Transport: httpx.HandlerTransport(busy)}, "admin.sock", io.Discard)
		assertErrorContains(t, err, "already running a login")
	})

	t.Run("an answer that is not the admin's is an error", func(t *testing.T) {
		h := newHarness(t, options{})
		err := remoteLogin(ctx, &http.Client{Transport: httpx.HandlerTransport(h.c.App)}, "treesinger.sock", io.Discard)
		assertErrorContains(t, err, "the broker refused the login: not_found (404 Not Found)")
	})

	for name, stream := range map[string]string{
		"a stream that ends without an outcome": prompt + "\n",
		"an event that is not JSON":             "{ok\n",
		"an event it does not know":             `{"progress":50}` + "\n",
	} {
		t.Run(name+" is an error", func(t *testing.T) {
			broken := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, stream) })
			if err := remoteLogin(ctx, &http.Client{Transport: httpx.HandlerTransport(broken)}, "admin.sock", io.Discard); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestLoginModes(t *testing.T) {
	ctx := context.Background()

	t.Run("with no admin socket, logs in here, straight into the store", func(t *testing.T) {
		h := newHarness(t, options{})
		scriptLogin(h)
		dir, _ := socketIn(t)
		missing := dir + "/admin.sock"
		var out bytes.Buffer

		err := Login(ctx, nil, missing, &out, func() (*Container, error) { return h.c, nil })

		assertEqual(t, err, nil)
		assertEqual(t, out.String(), "No broker admin socket at "+missing+", so logging in directly to /unused-in-tests.\n"+shownPrompt+
			"✓ Logged in. Refresh token stored. You can now `treesinger serve`.\n")
		stored, _ := h.store.Read()
		assertEqual(t, stored.RefreshToken, "refresh-token-1")
	})

	t.Run("a --socket that is not there is an error, never a fallback", func(t *testing.T) {
		dir, _ := socketIn(t)
		err := Login(ctx, []string{"--socket", dir + "/admin.sock"}, "/unused", io.Discard, noLocalLogin(t))
		assertErrorContains(t, err, "cannot use the broker's admin socket "+dir+"/admin.sock")
	})

	t.Run("a socket that is there but answers no one is an error, never a fallback", func(t *testing.T) {
		_, socket := socketIn(t)
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		listener.(*net.UnixListener).SetUnlinkOnClose(false)
		listener.Close() // left behind, as by a broker that died

		err = Login(ctx, nil, socket, io.Discard, noLocalLogin(t))

		assertErrorContains(t, err, "cannot use the broker's admin socket "+socket)
	})

	t.Run("something there that is not a socket is an error, never a fallback", func(t *testing.T) {
		_, path := socketIn(t)
		os.WriteFile(path, nil, 0o600)
		err := Login(ctx, nil, path, io.Discard, noLocalLogin(t))
		assertErrorContains(t, err, "cannot use the broker's admin socket")
	})

	t.Run("a local login that cannot be configured is an error", func(t *testing.T) {
		err := Login(ctx, nil, "/nonexistent/admin.sock", io.Discard, func() (*Container, error) { return nil, errors.New("no $HOME") })
		assertErrorContains(t, err, "no $HOME")
	})

	for name, args := range map[string][]string{
		"an argument":       {"now"},
		"an unknown option": {"--url", "http://x"},
		"an empty --socket": {"--socket", ""},
	} {
		t.Run(name+" is a usage error", func(t *testing.T) {
			var usage *UsageError
			err := Login(ctx, args, "/unused", io.Discard, noLocalLogin(t))
			assertEqual(t, errors.As(err, &usage), true)
			assertEqual(t, strings.HasPrefix(err.Error(), "usage: treesinger login"), true)
		})
	}
}
