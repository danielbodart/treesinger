package broker

import (
	"fmt"
	"math"
	"os"
	"os/user"
	"path/filepath"
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
// and setting both listens on both. An empty value counts as unset.
func ConfigFromEnv(env Env) (Config, error) {
	config := Config{
		Skew:        5 * time.Minute,
		Hostname:    "0.0.0.0",
		SocketPath:  env["TREESINGER_SOCKET"],
		OwnerUUID:   env["TREESINGER_OWNER_UUID"],
		BearerToken: env["TREESINGER_BEARER_TOKEN"],
	}
	if store, set := env["TREESINGER_STORE"]; set {
		config.StorePath = store
	} else {
		dir, err := configDir()
		if err != nil {
			return Config{}, err
		}
		config.StorePath = filepath.Join(dir, "treesinger", "tokens.json")
	}
	// Numbers are read as the TypeScript's Number() reads them (so "1e3",
	// "0x1F90" and "8080.0" are numbers), but one that is not finite, or that
	// a time.Duration cannot hold, fails at startup: NaN or Infinity would
	// otherwise overflow the skew and serve expired tokens.
	if value := env["TREESINGER_SKEW_MS"]; value != "" {
		ms := jsNumber(value)
		if math.IsNaN(ms) || math.IsInf(ms, 0) || math.Abs(ms) > float64(math.MaxInt64/int64(time.Millisecond)) {
			return Config{}, fmt.Errorf("TREESINGER_SKEW_MS must be a number of milliseconds, not %q", value)
		}
		config.Skew = time.Duration(ms * float64(time.Millisecond))
	}
	if value := env["TREESINGER_PORT"]; value != "" {
		number := jsNumber(value)
		if number != math.Trunc(number) || number < 0 || number > 65535 {
			return Config{}, fmt.Errorf("TREESINGER_PORT must be a port number, not %q", value)
		}
		port := int(number)
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

// configDir is $XDG_CONFIG_HOME when it is set at all, else ~/.config. With
// no $HOME (a bare `env -i`, a unit without User=) the home directory is the
// user's passwd entry, as Bun's os.homedir() finds it, never a path relative
// to wherever the process happens to be.
func configDir() (string, error) {
	if dir, set := os.LookupEnv("XDG_CONFIG_HOME"); set {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		account, lookup := user.Current()
		if lookup != nil || account.HomeDir == "" {
			return "", fmt.Errorf("no home directory for the token store (%v); set TREESINGER_STORE", err)
		}
		home = account.HomeDir
	}
	return filepath.Join(home, ".config"), nil
}
