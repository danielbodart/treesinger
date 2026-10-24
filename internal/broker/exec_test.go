package broker

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/danielbodart/gokit/environ"
)

// becoming records what Exec would have become, in place of syscall.Exec.
type becoming struct {
	called    bool
	path      string
	argv, env []string
}

func (b *becoming) become(path string, argv, env []string) error {
	b.called, b.path, b.argv, b.env = true, path, argv, env
	return nil
}

// command is a COMMAND that is certainly there to be found: this test binary.
func command(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// execute runs Exec with these arguments and environment, returning what it
// would have become and what it said.
func execute(t *testing.T, args []string, env environ.Env) (*becoming, string, error) {
	t.Helper()
	var stderr bytes.Buffer
	b := &becoming{}
	err := Exec(context.Background(), args, env, &stderr, b.become)
	return b, stderr.String(), err
}

func mintable(t *testing.T, configure func(*Config)) *harness {
	t.Helper()
	h := newHarness(t, options{seed: validSeed(), config: configure})
	h.fake.on(routeProfiles, scripted{json: fixture("profiles.json")}).on(routeNewSession, scripted{json: fixture("game-session.json")})
	return h
}

func withoutVar(env []string, name string) bool {
	return !slices.ContainsFunc(env, func(entry string) bool { return strings.HasPrefix(entry, name+"=") })
}

func TestExec(t *testing.T) {
	t.Run("mints, then becomes COMMAND with the pair in its environment, never its argv", func(t *testing.T) {
		socket := unixServer(t, mintable(t, nil).c.App)
		exe := command(t)

		b, said, err := execute(t, []string{"--socket", socket, "--", exe, "--world", "two words"}, environ.Env{"KEEP": "this"})

		assertEqual(t, err, nil)
		assertEqual(t, b.path, exe)
		assertEqual(t, strings.Join(b.argv, "|"), exe+"|--world|two words")
		assertEqual(t, strings.Join(b.env, " "), "HYTALE_SERVER_IDENTITY_TOKEN=identity-jwt-1 HYTALE_SERVER_SESSION_TOKEN=session-jwt-1 KEEP=this")
		assertEqual(t, said, "treesinger exec: authenticated via Treesinger ("+socket+")\n")
	})

	t.Run("--url mints over TCP", func(t *testing.T) {
		server := httptest.NewServer(mintable(t, nil).c.App)
		defer server.Close()

		b, said, err := execute(t, []string{"--url", server.URL + "/", "--", command(t)}, environ.Env{})

		assertEqual(t, err, nil)
		assertEqual(t, slices.Contains(b.env, "HYTALE_SERVER_SESSION_TOKEN=session-jwt-1"), true)
		assertEqual(t, said, "treesinger exec: authenticated via Treesinger ("+server.URL+"/)\n")
	})

	t.Run("sends the bearer token, and does not pass it on", func(t *testing.T) {
		socket := unixServer(t, mintable(t, func(c *Config) { c.BearerToken = "s3cret" }).c.App)

		b, _, err := execute(t, []string{"--socket", socket, "--", command(t)}, environ.Env{"TREESINGER_BEARER_TOKEN": "s3cret"})

		assertEqual(t, err, nil)
		assertEqual(t, slices.Contains(b.env, "HYTALE_SERVER_SESSION_TOKEN=session-jwt-1"), true)
		assertEqual(t, withoutVar(b.env, "TREESINGER_BEARER_TOKEN"), true)
	})

	t.Run("reads the bearer token from the systemd credential", func(t *testing.T) {
		socket := unixServer(t, mintable(t, func(c *Config) { c.BearerToken = "s3cret" }).c.App)

		b, _, err := execute(t, []string{"--socket", socket, "--", command(t)}, environ.Env{"CREDENTIALS_DIRECTORY": credentials(t, "s3cret\n")})

		assertEqual(t, err, nil)
		assertEqual(t, slices.Contains(b.env, "HYTALE_SERVER_SESSION_TOKEN=session-jwt-1"), true)
		// Passed on: it may hold COMMAND's own credentials, and withholding it
		// would not hide the file, which the unit can read either way (the
		// docs say so).
		assertEqual(t, withoutVar(b.env, "CREDENTIALS_DIRECTORY"), false)
	})

	t.Run("with no broker, warns and becomes COMMAND without a session", func(t *testing.T) {
		_, missing := socketIn(t)

		b, said, err := execute(t, []string{"--socket", missing, "--", command(t)}, environ.Env{"TREESINGER_BEARER_TOKEN": "s3cret", "KEEP": "this"})

		assertEqual(t, err, nil)
		assertEqual(t, b.called, true)
		assertEqual(t, strings.Join(b.env, " "), "KEEP=this") // and still not the bearer token
		assertEqual(t, strings.HasPrefix(said, "treesinger exec: WARNING: no session from Treesinger at "+missing+" ("), true)
		assertEqual(t, strings.HasSuffix(said, "); running unauthenticated\n"), true)
	})

	for name, test := range map[string]struct {
		handler http.Handler
		env     environ.Env
		says    string
	}{
		"not logged in": {newHarness(t, options{}).c.App, environ.Env{}, "(401 Unauthorized: not_logged_in)"},
		"the wrong bearer token": {mintable(t, func(c *Config) { c.BearerToken = "s3cret" }).c.App,
			environ.Env{"TREESINGER_BEARER_TOKEN": "wrong"}, "(401 Unauthorized: unauthorized)"},
		"an answer with no tokens":   {handlerSaying(`{"expiresAt":"2026-08-20T01:00:00Z"}`), environ.Env{}, "(the answer carried no tokens)"},
		"an answer that is not JSON": {handlerSaying("<html>"), environ.Env{}, "(an unreadable answer: "},
		"an empty bearer-token credential": {mintable(t, nil).c.App,
			environ.Env{"CREDENTIALS_DIRECTORY": credentials(t, "")}, "bearer-token credential"},
	} {
		t.Run(name+" warns and becomes COMMAND without a session", func(t *testing.T) {
			socket := unixServer(t, test.handler)

			b, said, err := execute(t, []string{"--socket", socket, "--", command(t)}, test.env)

			assertEqual(t, err, nil)
			assertEqual(t, b.called, true)
			assertEqual(t, withoutVar(b.env, sessionTokenVar) && withoutVar(b.env, identityTokenVar), true)
			if !strings.Contains(said, "WARNING") || !strings.Contains(said, test.says) {
				t.Errorf("said %q, want a warning with %q", said, test.says)
			}
		})
	}

	t.Run("a COMMAND that cannot be found is an error, with nothing minted or run", func(t *testing.T) {
		h := mintable(t, nil)
		socket := unixServer(t, h.c.App)

		b, _, err := execute(t, []string{"--socket", socket, "--", "/nonexistent/hytale-server"}, environ.Env{})

		if err == nil {
			t.Error("want an error")
		}
		assertEqual(t, b.called, false)
		assertEqual(t, h.fake.countOf(routeNewSession), 0)
	})

	t.Run("a COMMAND that cannot be run is an error", func(t *testing.T) {
		var stderr bytes.Buffer
		failing := func(string, []string, []string) error { return errors.New("exec format error") }
		_, missing := socketIn(t)
		err := Exec(context.Background(), []string{"--socket", missing, "--", command(t)}, environ.Env{}, &stderr, failing)
		assertErrorContains(t, err, "exec format error")
	})

	for name, args := range map[string][]string{
		"no --":                       {command(t)},
		"no COMMAND":                  {"--socket", "/s", "--"},
		"nothing at all":              {},
		"--socket and --url together": {"--socket", "/s", "--url", "http://x", "--", command(t)},
		"an unknown option":           {"--port", "8080", "--", command(t)},
		"an option without its value": {"--socket"},
	} {
		t.Run(name+" is a usage error, with nothing run", func(t *testing.T) {
			b, _, err := execute(t, args, environ.Env{})
			var usage *UsageError
			assertEqual(t, errors.As(err, &usage), true)
			assertEqual(t, b.called, false)
		})
	}
}

func TestBrokerOptionDefaults(t *testing.T) {
	line, err := parseCommandLine(execUsage, []string{"--", "server"}, "--socket", "--url")
	if err != nil {
		t.Fatal(err)
	}
	_, base, where, err := line.broker(execUsage, DefaultSocket)
	assertEqual(t, err, nil)
	assertEqual(t, where, "/run/treesinger/treesinger.sock")
	assertEqual(t, base, socketURL)
}
