package broker

import (
	"maps"
	"testing"
	"time"
)

// withHome is env with a home directory, which every configuration needs for
// its default store path.
func withHome(env Env) Env {
	home := Env{"HOME": "/home/op"}
	maps.Copy(home, env)
	return home
}

func TestConfigFromEnvListeners(t *testing.T) {
	tests := []struct {
		name       string
		env        Env
		port       *int
		hostname   string
		socketPath string
	}{
		{"nothing set is TCP 8080 on every address, as the image has always been",
			Env{}, port(8080), "0.0.0.0", ""},
		{"a socket alone means no TCP listener",
			Env{"TREESINGER_SOCKET": "/run/treesinger/treesinger.sock"}, nil, "0.0.0.0", "/run/treesinger/treesinger.sock"},
		{"a socket and a port listen on both",
			Env{"TREESINGER_SOCKET": "/run/t.sock", "TREESINGER_PORT": "9090"}, port(9090), "0.0.0.0", "/run/t.sock"},
		{"the TCP bind address is honoured",
			Env{"TREESINGER_HOST": "127.0.0.1"}, port(8080), "127.0.0.1", ""},
		{"empty values count as unset",
			Env{"TREESINGER_SOCKET": "", "TREESINGER_HOST": ""}, port(8080), "0.0.0.0", ""},
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
			assertEqual(t, config.SocketPath, test.socketPath)
		})
	}
}

func TestConfigFromEnvStorePath(t *testing.T) {
	tests := []struct {
		name string
		env  Env
		want string
	}{
		{"set explicitly", Env{"TREESINGER_STORE": "/data/tokens.json", "HOME": "/home/op"}, "/data/tokens.json"},
		{"under $XDG_CONFIG_HOME", Env{"XDG_CONFIG_HOME": "/xdg", "HOME": "/home/op"}, "/xdg/treesinger/tokens.json"},
		{"else under ~/.config", Env{"HOME": "/home/op"}, "/home/op/.config/treesinger/tokens.json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, err := ConfigFromEnv(test.env)
			assertEqual(t, err, nil)
			assertEqual(t, config.StorePath, test.want)
		})
	}

	t.Run("with nowhere to put it, startup fails rather than guessing", func(t *testing.T) {
		_, err := ConfigFromEnv(Env{})
		assertErrorContains(t, err, "TREESINGER_STORE")
	})
}

func TestConfigFromEnvNumbers(t *testing.T) {
	t.Run("the skew is read in milliseconds", func(t *testing.T) {
		config, err := ConfigFromEnv(withHome(Env{"TREESINGER_SKEW_MS": "60000"}))
		assertEqual(t, err, nil)
		assertEqual(t, config.Skew, time.Minute)
	})

	for name, env := range map[string]Env{
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
