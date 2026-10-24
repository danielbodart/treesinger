package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os/exec"
	"time"

	"github.com/danielbodart/gokit/environ"
)

// DefaultSocket is where treesinger.socket listens, the directory around it
// being what a confined game server has bind-mounted.
const DefaultSocket = "/run/treesinger/treesinger.sock"

const execUsage = "exec [--socket PATH | --url URL] -- COMMAND [ARGS...]"

// Become replaces this process with another, as syscall.Exec does; tests
// record what it was asked instead.
type Become func(path string, argv, env []string) error

// The variables a Hytale server reads its session from itself, so the tokens
// never touch argv (where every user on the host can read them).
const (
	sessionTokenVar  = "HYTALE_SERVER_SESSION_TOKEN"
	identityTokenVar = "HYTALE_SERVER_IDENTITY_TOKEN"
)

// Exec is `treesinger exec [--socket PATH | --url URL] -- COMMAND [ARGS...]`:
// mint a session from a broker (the socket at DefaultSocket unless told
// otherwise), then become COMMAND with it in the environment. For a systemd
// unit's ExecStart, so the tokens never land in a file, and it is the same
// POST a Hytale image makes to TREESINGER_URL.
//
// Any failure to mint (no broker, not logged in, a bad answer) warns and runs
// COMMAND without them: an unauthenticated boot, recoverable, rather than no
// boot at all. The bearer token, if one is configured, is sent, and
// TREESINGER_BEARER_TOKEN is removed from COMMAND's environment; a
// bearer-token credential cannot be withheld that way (its file stays readable
// by the unit, so by COMMAND), and CREDENTIALS_DIRECTORY, which may hold
// COMMAND's own credentials, is passed on. Becoming COMMAND, rather than
// starting it, is what lets it keep this process's pid, stdin and notify
// socket.
//
// Only a command line that does not parse, or a COMMAND that cannot be found
// or run, is an error, returned without running anything.
func Exec(ctx context.Context, args []string, env environ.Env, stderr io.Writer, become Become) error {
	line, err := parseCommandLine(execUsage, args, "--socket", "--url")
	if err != nil {
		return err
	}
	if !line.dashDash || len(line.rest) == 0 {
		return usageError(execUsage)
	}
	client, base, where, err := line.broker(execUsage, DefaultSocket)
	if err != nil {
		return err
	}
	// Found first, so a typo costs no session. On this process's PATH, which
	// is the one COMMAND inherits.
	path, err := exec.LookPath(line.rest[0])
	if err != nil {
		return err
	}

	environment := maps.Clone(env)
	delete(environment, "TREESINGER_BEARER_TOKEN")
	session, err := mint(ctx, client, base, env)
	if err != nil {
		fmt.Fprintf(stderr, "treesinger exec: WARNING: no session from Treesinger at %s (%v); running unauthenticated\n", where, err)
	} else {
		environment[sessionTokenVar], environment[identityTokenVar] = session.SessionToken, session.IdentityToken
		fmt.Fprintf(stderr, "treesinger exec: authenticated via Treesinger (%s)\n", where)
	}
	if err := become(path, line.rest, environment.List()); err != nil {
		return fmt.Errorf("exec %s: %w", path, err)
	}
	return nil
}

// mintTimeout bounds the whole mint, so a broker that accepts but never
// answers delays a boot by this much, not forever.
const mintTimeout = 10 * time.Second

// mint is one POST /v1/session, with the bearer token if one is configured.
func mint(ctx context.Context, client *http.Client, base string, env environ.Env) (GameSession, error) {
	var session GameSession
	token, err := BearerTokenFromEnv(env)
	if err != nil {
		return session, err
	}
	ctx, cancel := context.WithTimeout(ctx, mintTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/session", nil)
	if err != nil {
		return session, err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return session, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes))
	if err != nil {
		return session, err
	}
	if !succeeded(response) {
		return session, fmt.Errorf("%s: %s", response.Status, errorCode(data))
	}
	if err := json.Unmarshal(data, &session); err != nil {
		return session, fmt.Errorf("an unreadable answer: %w", err)
	}
	if session.SessionToken == "" || session.IdentityToken == "" {
		return session, errors.New("the answer carried no tokens")
	}
	return session, nil
}
