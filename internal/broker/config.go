package broker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Config is static runtime configuration, sourced from the environment. All
// values have sensible defaults except the refresh token, which a login seeds
// into the store separately.
type Config struct {
	// Where the token store lives on the mounted volume.
	StorePath string
	// Refresh the OAuth access token when less than this much life remains.
	Skew time.Duration
	// TCP port `serve` listens on when systemd has not handed it sockets, or
	// nil for the default, 8080.
	Port *int
	// Address that TCP listener binds, or "" for the default, every IPv4
	// address.
	Hostname string
	// Preferred profile to mint sessions for when the account has more than
	// one. Unset → the first profile from `get-profiles`. A request body may
	// override per-call.
	OwnerUUID string
	// Optional shared secret for `/v1/session`. Off by default: the boundary is
	// Docker network isolation, or the socket's group; this is opt-in hardening.
	BearerToken string
}

// DefaultPort is where `serve` listens, and `health` probes, when
// TREESINGER_PORT does not say otherwise: what the Docker image has always
// used.
const DefaultPort = 8080

// Env is an environment as a map, so tests pass one instead of mutating the
// process's.
type Env map[string]string

// Environ is the process's environment.
func Environ() Env {
	env := Env{}
	for _, entry := range os.Environ() {
		if key, value, found := strings.Cut(entry, "="); found {
			env[key] = value
		}
	}
	return env
}

// ConfigFromEnv reads everything `serve` needs. An empty value counts as
// unset, and a value that does not parse fails at startup rather than being
// guessed at. Where it listens is not decided here: that depends on whether
// systemd handed it sockets (see Listen).
func ConfigFromEnv(env Env) (Config, error) {
	// The unit of releases before socket activation sets it. Under that unit
	// this broker would get no sockets from systemd and fall back to TCP on
	// every interface, the unix socket its clients use gone, so it stops instead.
	if _, set := env["TREESINGER_SOCKET"]; set {
		return Config{}, errors.New("TREESINGER_SOCKET is no longer read: the client socket comes from treesinger.socket now; install treesinger.socket, treesinger-admin.socket and the new treesinger.service (see docs/systemd.md, Upgrade)")
	}
	config := Config{
		StorePath: env["TREESINGER_STORE"],
		Skew:      5 * time.Minute,
		OwnerUUID: env["TREESINGER_OWNER_UUID"],
	}
	if config.StorePath == "" {
		dir, err := configDir(env)
		if err != nil {
			return Config{}, err
		}
		config.StorePath = filepath.Join(dir, "treesinger", "tokens.json")
	}
	if value := env["TREESINGER_SKEW_MS"]; value != "" {
		ms, err := strconv.ParseInt(value, 10, 64)
		// Bounded so the duration cannot overflow into a negative skew, which
		// would hand out tokens past their expiry.
		if err != nil || ms < 0 || ms > int64(time.Duration(1<<62)/time.Millisecond) {
			return Config{}, fmt.Errorf("TREESINGER_SKEW_MS must be a whole number of milliseconds, not %q", value)
		}
		config.Skew = time.Duration(ms) * time.Millisecond
	}
	var err error
	if config.Port, config.Hostname, err = tcpFromEnv(env); err != nil {
		return Config{}, err
	}
	if config.BearerToken, err = BearerTokenFromEnv(env); err != nil {
		return Config{}, err
	}
	return config, nil
}

// tcpFromEnv is TREESINGER_PORT and TREESINGER_HOST, each nil or "" when unset
// so that a caller can tell "unset" from "set to the default".
func tcpFromEnv(env Env) (*int, string, error) {
	value := env["TREESINGER_PORT"]
	if value == "" {
		return nil, env["TREESINGER_HOST"], nil
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 0 || port > 65535 {
		return nil, "", fmt.Errorf("TREESINGER_PORT must be a port number, not %q", value)
	}
	return &port, env["TREESINGER_HOST"], nil
}

// BearerTokenFromEnv is the shared secret for `/v1/session`, for the broker
// that checks it and for `exec`, which sends it: TREESINGER_BEARER_TOKEN, else
// the systemd credential `bearer-token` (LoadCredential=bearer-token:PATH),
// which keeps the secret out of the unit's environment and needs no wrapper
// to read it. "" when neither is set, which turns the gate off.
func BearerTokenFromEnv(env Env) (string, error) {
	if token := env["TREESINGER_BEARER_TOKEN"]; token != "" {
		return token, checkToken("TREESINGER_BEARER_TOKEN", token)
	}
	dir := env["CREDENTIALS_DIRECTORY"]
	if dir == "" {
		return "", nil
	}
	path := filepath.Join(dir, "bearer-token")
	data, err := os.ReadFile(path)
	// The directory holds whatever credentials the unit loads; this one is
	// optional.
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// The newline an editor or `echo` leaves is not part of the secret.
	token := strings.TrimRight(string(data), "\r\n")
	if token == "" {
		// A credential deliberately loaded but empty is a mistake, and taken
		// as "no token" it would quietly open the gate.
		return "", fmt.Errorf("the bearer-token credential (%s) is empty", path)
	}
	return token, checkToken(path, token)
}

// checkToken refuses a token that cannot be sent in a header as it stands, so
// the mistake shows at startup rather than as every request failing.
func checkToken(source, token string) error {
	if strings.ContainsFunc(token, unicode.IsControl) {
		return fmt.Errorf("the bearer token in %s contains control characters", source)
	}
	return nil
}

// configDir is $XDG_CONFIG_HOME, else $HOME/.config. With neither there is no
// sensible default, and a path relative to wherever the process happens to be
// would be worse than none.
func configDir(env Env) (string, error) {
	if dir := env["XDG_CONFIG_HOME"]; dir != "" {
		return dir, nil
	}
	if home := env["HOME"]; home != "" {
		return filepath.Join(home, ".config"), nil
	}
	return "", errors.New("no $XDG_CONFIG_HOME or $HOME for the token store; set TREESINGER_STORE")
}
