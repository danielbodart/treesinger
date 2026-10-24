package broker

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielbodart/gokit/environ"
)

// withHome is env with a home directory, which every configuration needs for
// its default store path.
func withHome(env environ.Env) environ.Env {
	home := environ.Env{"HOME": "/home/op"}
	maps.Copy(home, env)
	return home
}

func TestConfigFromEnvListeners(t *testing.T) {
	tests := []struct {
		name     string
		env      environ.Env
		port     *int
		hostname string
	}{
		{"nothing set is left unset, for Listen to default (or refuse under socket activation)",
			environ.Env{}, nil, ""},
		{"the TCP port is read",
			environ.Env{"TREESINGER_PORT": "9090"}, port(9090), ""},
		{"the TCP bind address is read",
			environ.Env{"TREESINGER_HOST": "127.0.0.1"}, nil, "127.0.0.1"},
		{"empty values count as unset",
			environ.Env{"TREESINGER_PORT": "", "TREESINGER_HOST": ""}, nil, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, err := ConfigFromEnv(withHome(test.env))
			if err != nil {
				t.Fatal(err)
			}
			if (config.Port == nil) != (test.port == nil) || (config.Port != nil && *config.Port != *test.port) {
				t.Errorf("port: got %v, want %v", deref(config.Port), deref(test.port))
			}
			assertEqual(t, config.Hostname, test.hostname)
		})
	}
}

// credentials is a systemd credentials directory holding a bearer-token
// file with these contents.
func credentials(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bearer-token"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBearerToken(t *testing.T) {
	t.Run("is off when nothing configures one", func(t *testing.T) {
		config, err := ConfigFromEnv(withHome(environ.Env{}))
		assertEqual(t, err, nil)
		assertEqual(t, config.BearerToken, "")
	})

	t.Run("is read from TREESINGER_BEARER_TOKEN", func(t *testing.T) {
		config, err := ConfigFromEnv(withHome(environ.Env{"TREESINGER_BEARER_TOKEN": "s3cret"}))
		assertEqual(t, err, nil)
		assertEqual(t, config.BearerToken, "s3cret")
	})

	t.Run("else from the bearer-token credential, without its trailing newline", func(t *testing.T) {
		config, err := ConfigFromEnv(withHome(environ.Env{"CREDENTIALS_DIRECTORY": credentials(t, "from-a-file\n")}))
		assertEqual(t, err, nil)
		assertEqual(t, config.BearerToken, "from-a-file")
	})

	t.Run("the variable wins over the credential", func(t *testing.T) {
		token, err := BearerTokenFromEnv(environ.Env{"TREESINGER_BEARER_TOKEN": "variable", "CREDENTIALS_DIRECTORY": credentials(t, "file")})
		assertEqual(t, err, nil)
		assertEqual(t, token, "variable")
	})

	t.Run("a credentials directory without one is no token", func(t *testing.T) {
		token, err := BearerTokenFromEnv(environ.Env{"CREDENTIALS_DIRECTORY": t.TempDir()})
		assertEqual(t, err, nil)
		assertEqual(t, token, "")
	})

	t.Run("an empty credential fails at startup rather than opening the gate", func(t *testing.T) {
		_, err := ConfigFromEnv(withHome(environ.Env{"CREDENTIALS_DIRECTORY": credentials(t, "\n")}))
		assertErrorContains(t, err, "is empty")
	})

	t.Run("a token that cannot be sent in a header fails at startup", func(t *testing.T) {
		_, err := BearerTokenFromEnv(environ.Env{"CREDENTIALS_DIRECTORY": credentials(t, "two\nlines\n")})
		assertErrorContains(t, err, "control characters")
		_, err = BearerTokenFromEnv(environ.Env{"TREESINGER_BEARER_TOKEN": "tab\there"})
		assertErrorContains(t, err, "control characters")
	})
}

func TestConfigFromEnvStorePath(t *testing.T) {
	tests := []struct {
		name string
		env  environ.Env
		want string
	}{
		{"set explicitly", environ.Env{"TREESINGER_STORE": "/data/tokens.json", "HOME": "/home/op"}, "/data/tokens.json"},
		{"under $XDG_CONFIG_HOME", environ.Env{"XDG_CONFIG_HOME": "/xdg", "HOME": "/home/op"}, "/xdg/treesinger/tokens.json"},
		{"else under ~/.config", environ.Env{"HOME": "/home/op"}, "/home/op/.config/treesinger/tokens.json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, err := ConfigFromEnv(test.env)
			assertEqual(t, err, nil)
			assertEqual(t, config.StorePath, test.want)
		})
	}

	t.Run("with nowhere to put it, startup fails rather than guessing", func(t *testing.T) {
		_, err := ConfigFromEnv(environ.Env{})
		assertErrorContains(t, err, "TREESINGER_STORE")
	})
}

func TestConfigFromEnvNumbers(t *testing.T) {
	t.Run("the skew is read in milliseconds", func(t *testing.T) {
		config, err := ConfigFromEnv(withHome(environ.Env{"TREESINGER_SKEW_MS": "60000"}))
		assertEqual(t, err, nil)
		assertEqual(t, config.Skew, time.Minute)
	})

	for name, env := range map[string]environ.Env{
		"a port that is not a number":       {"TREESINGER_PORT": "http"},
		"a port out of range":               {"TREESINGER_PORT": "65536"},
		"a skew that is not a number":       {"TREESINGER_SKEW_MS": "5m"},
		"a negative skew":                   {"TREESINGER_SKEW_MS": "-1"},
		"a skew too long for time.Duration": {"TREESINGER_SKEW_MS": "9223372036854775807"},
	} {
		t.Run(name+" fails at startup", func(t *testing.T) {
			_, err := ConfigFromEnv(withHome(env))
			if err == nil {
				t.Error("want an error")
			}
		})
	}
}

func deref(p *int) any {
	if p == nil {
		return "unset"
	}
	return *p
}

func TestConfigFromEnvRefusesTheOldSocketVariable(t *testing.T) {
	// A pre-socket-activation unit sets it; served under that unit, the broker
	// would quietly fall back to TCP on 0.0.0.0 with no unix socket.
	for _, value := range []string{"/run/treesinger/treesinger.sock", ""} {
		_, err := ConfigFromEnv(withHome(environ.Env{"TREESINGER_SOCKET": value}))
		if err == nil || !strings.Contains(err.Error(), "treesinger.socket") {
			t.Fatalf("TREESINGER_SOCKET=%q: got %v", value, err)
		}
	}
}
