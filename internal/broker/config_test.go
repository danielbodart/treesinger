package broker

import "testing"

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
			config, err := ConfigFromEnv(test.env)
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

func deref(p *int) any {
	if p == nil {
		return "unset"
	}
	return *p
}
