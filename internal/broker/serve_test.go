package broker

import (
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// socketIn is a socket path in a fresh directory. Short on purpose: a unix
// socket path has to fit in 108 bytes, which t.TempDir's names can outgrow.
func socketIn(t *testing.T) (dir, socketPath string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ts-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir, filepath.Join(dir, "treesinger.sock")
}

type brokerOptions struct {
	socketPath string
	port       *int
	seeded     bool
}

// broker is a broker on real listeners, its upstream the usual fake.
func broker(t *testing.T, o brokerOptions) (*harness, *Listening) {
	t.Helper()
	var seed *StoredTokens
	if o.seeded {
		seed = validSeed()
	}
	h := newHarness(t, options{seed: seed, config: func(c *Config) { c.SocketPath, c.Port = o.socketPath, o.port }})
	listening, err := Listen(h.config, h.c.App)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(listening.Stop)
	return h, listening
}

// stale leaves a socket at path the way a crash does: bound, then its process
// gone without unlinking it.
func stale(t *testing.T, path string) {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	listener.Close()
}

func overSocket(t *testing.T, socketPath, method, path string) *http.Response {
	t.Helper()
	client := &http.Client{Transport: unixTransport(socketPath)}
	request, _ := http.NewRequest(method, "http://treesinger"+path, nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func body(t *testing.T, response *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func gone(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
}

func isSocket(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&fs.ModeSocket != 0
}

func assertErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("want an error containing %q, got %v", want, err)
	}
}

func TestUnixSocketListener(t *testing.T) {
	t.Run("serves the API: health 503 and not_logged_in before any login", func(t *testing.T) {
		_, socketPath := socketIn(t)
		broker(t, brokerOptions{socketPath: socketPath})

		health := overSocket(t, socketPath, http.MethodGet, "/health")
		assertEqual(t, health.StatusCode, 503)
		assertJSON(t, body(t, health), `{"status":"unauthenticated"}`)

		session := overSocket(t, socketPath, http.MethodPost, "/v1/session")
		assertEqual(t, session.StatusCode, 401)
		assertJSON(t, body(t, session), `{"error":"not_logged_in"}`)
	})

	t.Run("is mode 0660 whatever the umask", func(t *testing.T) {
		_, socketPath := socketIn(t)
		previous := syscall.Umask(0o022)
		broker(t, brokerOptions{socketPath: socketPath})
		syscall.Umask(previous)

		info, _ := os.Stat(socketPath)
		assertEqual(t, info.Mode().Perm(), 0o660)
	})

	t.Run("there is no TCP listener when only a socket is configured", func(t *testing.T) {
		_, socketPath := socketIn(t)
		_, listening := broker(t, brokerOptions{socketPath: socketPath})
		assertEqual(t, strings.Join(listening.URLs, " "), "unix:"+socketPath)
	})

	t.Run("a stale socket left by a dead broker is replaced", func(t *testing.T) {
		_, socketPath := socketIn(t)
		stale(t, socketPath)
		assertEqual(t, isSocket(socketPath), true)

		broker(t, brokerOptions{socketPath: socketPath})

		assertEqual(t, overSocket(t, socketPath, http.MethodGet, "/health").StatusCode, 503)
	})

	t.Run("a live socket is not stolen from the broker answering on it", func(t *testing.T) {
		_, socketPath := socketIn(t)
		broker(t, brokerOptions{socketPath: socketPath})
		second := newHarness(t, options{config: func(c *Config) { c.SocketPath, c.Port = socketPath, nil }})

		_, err := Listen(second.config, second.c.App)

		assertErrorContains(t, err, "already being served")
		assertEqual(t, overSocket(t, socketPath, http.MethodGet, "/health").StatusCode, 503) // the first still answers
	})

	t.Run("anything that is not a socket is refused and left intact", func(t *testing.T) {
		_, socketPath := socketIn(t)
		os.WriteFile(socketPath, []byte("someone's file"), 0o644)
		h := newHarness(t, options{config: func(c *Config) { c.SocketPath, c.Port = socketPath, nil }})

		_, err := Listen(h.config, h.c.App)

		assertErrorContains(t, err, "not a socket")
		data, _ := os.ReadFile(socketPath)
		assertEqual(t, string(data), "someone's file")
	})

	t.Run("stopping removes the socket", func(t *testing.T) {
		_, socketPath := socketIn(t)
		_, listening := broker(t, brokerOptions{socketPath: socketPath})
		listening.Stop()
		assertEqual(t, gone(socketPath), true)
	})
}

func TestSocketAndTCPTogether(t *testing.T) {
	t.Run("both answer", func(t *testing.T) {
		_, socketPath := socketIn(t)
		_, listening := broker(t, brokerOptions{socketPath: socketPath, port: port(0), seeded: true})

		assertEqual(t, len(listening.URLs), 2)
		assertEqual(t, listening.URLs[0], "unix:"+socketPath)
		assertEqual(t, overSocket(t, socketPath, http.MethodGet, "/health").StatusCode, 200)
		response, err := http.Get(listening.URLs[1] + "/health")
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		assertEqual(t, response.StatusCode, 200)
	})

	t.Run("a TCP bind failure closes the socket it already opened", func(t *testing.T) {
		_, socketPath := socketIn(t)
		blocker, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Close()
		h := newHarness(t, options{config: func(c *Config) {
			c.SocketPath, c.Port = socketPath, port(blocker.Addr().(*net.TCPAddr).Port)
		}})

		_, err = Listen(h.config, h.c.App)

		if err == nil {
			t.Fatal("want a bind failure")
		}
		assertEqual(t, gone(socketPath), true)
	})
}

func TestHealthCommand(t *testing.T) {
	t.Run("probes the socket when one is configured", func(t *testing.T) {
		_, socketPath := socketIn(t)
		h, _ := broker(t, brokerOptions{socketPath: socketPath, seeded: true})
		assertEqual(t, Health(h.config), 0)
	})

	t.Run("is unhealthy over the socket when no one has logged in", func(t *testing.T) {
		_, socketPath := socketIn(t)
		h, _ := broker(t, brokerOptions{socketPath: socketPath})
		assertEqual(t, Health(h.config), 1)
	})

	t.Run("probes loopback TCP when the listener binds every address", func(t *testing.T) {
		_, listening := broker(t, brokerOptions{port: port(0), seeded: true})
		bound, _ := url.Parse(listening.URLs[0])
		p, _ := strconv.Atoi(bound.Port())
		h := newHarness(t, options{seed: validSeed(), config: func(c *Config) { c.Port, c.Hostname = port(p), "0.0.0.0" }})
		assertEqual(t, Health(h.config), 0)
	})

	t.Run("fails when nothing is listening", func(t *testing.T) {
		_, socketPath := socketIn(t)
		h := newHarness(t, options{config: func(c *Config) { c.SocketPath = socketPath }})
		assertEqual(t, Health(h.config), 1)
	})
}

func TestReadiness(t *testing.T) {
	t.Run("is signalled once, after the socket is bound", func(t *testing.T) {
		_, socketPath := socketIn(t)
		var calls []bool
		notify := func(Env) error {
			calls = append(calls, isSocket(socketPath))
			return nil
		}
		h := newHarness(t, options{config: func(c *Config) { c.SocketPath, c.Port = socketPath, nil }})

		listening, err := Start(h.c, notify, Env{"NOTIFY_SOCKET": "/run/systemd/notify"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(listening.Stop)

		assertEqual(t, len(calls), 1)
		assertEqual(t, calls[0], true)
	})

	t.Run("a failed notify does not leave the broker listening", func(t *testing.T) {
		_, socketPath := socketIn(t)
		h := newHarness(t, options{config: func(c *Config) { c.SocketPath, c.Port = socketPath, nil }})
		failing := func(Env) error { return errors.New("no systemd") }

		_, err := Start(h.c, failing, Environ())

		assertErrorContains(t, err, "no systemd")
		assertEqual(t, gone(socketPath), true)
	})
}

func TestSystemdNotify(t *testing.T) {
	t.Run("does nothing outside systemd", func(t *testing.T) {
		assertEqual(t, SystemdNotify(Env{}), nil) // no NOTIFY_SOCKET: nothing sent, nothing returned
	})

	t.Run("fails loudly when systemd's socket cannot be reached", func(t *testing.T) {
		dir, _ := socketIn(t)
		if SystemdNotify(Env{"NOTIFY_SOCKET": filepath.Join(dir, "missing")}) == nil {
			t.Error("want an error")
		}
	})

	t.Run("sends READY=1 as one datagram to systemd's socket", func(t *testing.T) {
		dir, _ := socketIn(t)
		path := filepath.Join(dir, "notify")
		systemd, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
		if err != nil {
			t.Fatal(err)
		}
		defer systemd.Close()

		if err := SystemdNotify(Env{"NOTIFY_SOCKET": path}); err != nil {
			t.Fatal(err)
		}

		datagram := make([]byte, 64)
		n, _, err := systemd.ReadFromUnix(datagram)
		if err != nil {
			t.Fatal(err)
		}
		assertEqual(t, string(datagram[:n]), "READY=1")
	})
}
