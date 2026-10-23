package broker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is static runtime configuration, sourced from the environment. All
// values have sensible defaults except the refresh token, which the `login`
// command seeds into the store separately.
type Config struct {
	// Where the token store lives on the mounted volume.
	StorePath string
	// Refresh the OAuth access token when less than this much life remains.
	Skew time.Duration
	// TCP port `serve` listens on, or nil for no TCP listener.
	Port *int
	// Address the TCP listener binds.
	Hostname string
	// Unix socket `serve` listens on, or "" for none. Created by the broker
	// itself, mode 0660 with the process's primary group: that group is the
	// access boundary.
	SocketPath string
	// Preferred profile to mint sessions for when the account has more than
	// one. Unset → the first profile from `get-profiles`. A request body may
	// override per-call.
	OwnerUUID string
	// Optional shared secret for `/v1/session`. Off by default: the boundary is
	// Docker network isolation, or the socket's group; this is opt-in hardening.
	BearerToken string
}

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

// ConfigFromEnv keeps TCP on 8080 unless a socket is configured, so the Docker
// image (which sets neither) is unchanged; a socket alone means no TCP at all,
// and setting both listens on both. An empty value counts as unset, and a
// value that does not parse fails at startup rather than being guessed at.
func ConfigFromEnv(env Env) (Config, error) {
	config := Config{
		StorePath:   env["TREESINGER_STORE"],
		Skew:        5 * time.Minute,
		Hostname:    "0.0.0.0",
		SocketPath:  env["TREESINGER_SOCKET"],
		OwnerUUID:   env["TREESINGER_OWNER_UUID"],
		BearerToken: env["TREESINGER_BEARER_TOKEN"],
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
	if value := env["TREESINGER_PORT"]; value != "" {
		port, err := strconv.Atoi(value)
		if err != nil || port < 0 || port > 65535 {
			return Config{}, fmt.Errorf("TREESINGER_PORT must be a port number, not %q", value)
		}
		config.Port = &port
	} else if config.SocketPath == "" {
		port := 8080
		config.Port = &port
	}
	if host := env["TREESINGER_HOST"]; host != "" {
		config.Hostname = host
	}
	return config, nil
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
