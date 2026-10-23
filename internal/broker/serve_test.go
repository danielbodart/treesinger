package broker

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// unixServer serves handler on a fresh unix socket, as the broker behind
// treesinger.socket would, and is the socket's path.
func unixServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	_, path := socketIn(t)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return path
}

func overSocket(t *testing.T, socketPath, method, path string) *http.Response {
	t.Helper()
	client := &http.Client{Transport: unixTransport(socketPath)}
	request, _ := http.NewRequest(method, socketURL+path, nil)
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

// passedSocket is one socket for systemd to pass: its descriptor name, and
// "unix" or "tcp".
type passedSocket struct{ name, network string }

// passed is what systemd hands a socket-activated process: real listening
// sockets, made here, as descriptors 3, 4, … with LISTEN_* saying so.
type passed struct {
	env Env
	// Stands in for os.NewFile over the inherited descriptors.
	file func(fd uintptr, name string) *os.File
	// Each socket as a URL a client could use, in descriptor order.
	urls []string
	// The unix sockets' paths, in descriptor order, "" for a TCP one.
	paths []string
}

const testPid = 4242

func passing(t *testing.T, sockets ...passedSocket) passed {
	t.Helper()
	p := passed{env: Env{"LISTEN_PID": strconv.Itoa(testPid), "LISTEN_FDS": strconv.Itoa(len(sockets))}}
	files := map[uintptr]*os.File{}
	var names []string
	for i, socket := range sockets {
		var listener net.Listener
		var err error
		path := ""
		if socket.network == "unix" {
			_, path = socketIn(t)
			listener, err = net.Listen("unix", path)
		} else {
			listener, err = net.Listen("tcp4", "127.0.0.1:0")
		}
		if err != nil {
			t.Fatal(err)
		}
		file, err := listener.(interface{ File() (*os.File, error) }).File()
		if err != nil {
			t.Fatal(err)
		}
		// Only the descriptor passes on, as from systemd, which keeps the
		// socket's file: closing this end must not unlink it.
		if unix, ok := listener.(*net.UnixListener); ok {
			unix.SetUnlinkOnClose(false)
		}
		listener.Close()
		t.Cleanup(func() { file.Close() }) // when the test never takes it
		files[uintptr(listenFdsStart+i)] = file
		names = append(names, socket.name)
		p.urls = append(p.urls, label(listener))
		p.paths = append(p.paths, path)
	}
	p.env["LISTEN_FDNAMES"] = strings.Join(names, ":")
	p.file = func(fd uintptr, _ string) *os.File { return files[fd] }
	return p
}

// handlerSaying answers every request with its own name, so a test can tell
// which handler a listener was given.
func handlerSaying(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, name) })
}

func get(t *testing.T, url string) string {
	t.Helper()
	if path, ok := strings.CutPrefix(url, "unix:"); ok {
		return body(t, overSocket(t, path, http.MethodGet, "/"))
	}
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return body(t, response)
}

func TestActivated(t *testing.T) {
	t.Run("is no activation without LISTEN_PID", func(t *testing.T) {
		inherited, err := Activated(Env{}, testPid, nil)
		assertEqual(t, err, nil)
		assertEqual(t, len(inherited), 0)
	})

	t.Run("ignores sockets meant for another process, and leaves them be", func(t *testing.T) {
		p := passing(t, passedSocket{"treesinger.socket", "unix"})
		inherited, err := Activated(p.env, testPid+1, func(uintptr, string) *os.File {
			t.Error("a descriptor meant for another process was taken")
			return nil
		})
		assertEqual(t, err, nil)
		assertEqual(t, len(inherited), 0)
	})

	t.Run("takes LISTEN_FDS descriptors from 3, named by LISTEN_FDNAMES", func(t *testing.T) {
		p := passing(t, passedSocket{"treesinger.socket", "unix"}, passedSocket{"admin", "unix"}, passedSocket{"treesinger.socket", "tcp"})

		inherited, err := Activated(p.env, testPid, p.file)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { closeAll(inherited) })

		assertEqual(t, len(inherited), 3)
		for i, want := range []string{"treesinger.socket", "admin", "treesinger.socket"} {
			assertEqual(t, inherited[i].Name, want)
			assertEqual(t, label(inherited[i].Listener), p.urls[i])
		}
	})

	t.Run("an unnamed descriptor is \"unknown\", as systemd's own default", func(t *testing.T) {
		p := passing(t, passedSocket{"ignored", "tcp"})
		delete(p.env, "LISTEN_FDNAMES")

		inherited, err := Activated(p.env, testPid, p.file)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { closeAll(inherited) })
		assertEqual(t, inherited[0].Name, "unknown")
	})

	t.Run("LISTEN_FDNAMES disagreeing with LISTEN_FDS fails", func(t *testing.T) {
		p := passing(t, passedSocket{"a", "tcp"})
		p.env["LISTEN_FDNAMES"] = "a:b"
		_, err := Activated(p.env, testPid, p.file)
		assertErrorContains(t, err, "LISTEN_FDNAMES names 2 descriptors but LISTEN_FDS passes 1")
	})

	for name, env := range map[string]Env{
		"a LISTEN_PID that is not a pid":       {"LISTEN_PID": "me", "LISTEN_FDS": "1"},
		"a LISTEN_FDS that is not a count":     {"LISTEN_PID": strconv.Itoa(testPid), "LISTEN_FDS": "one"},
		"a negative LISTEN_FDS":                {"LISTEN_PID": strconv.Itoa(testPid), "LISTEN_FDS": "-1"},
		"LISTEN_FDS without LISTEN_PID":        {"LISTEN_FDS": "1"},
		"LISTEN_PID for us without LISTEN_FDS": {"LISTEN_PID": strconv.Itoa(testPid)},
	} {
		t.Run(name+" fails", func(t *testing.T) {
			if _, err := Activated(env, testPid, func(uintptr, string) *os.File { return nil }); err == nil {
				t.Error("want an error")
			}
		})
	}

	t.Run("a descriptor that is not a socket fails, and closes the ones taken before it", func(t *testing.T) {
		p := passing(t, passedSocket{"treesinger.socket", "unix"}, passedSocket{"oops", "unix"})
		notASocket, err := os.CreateTemp(t.TempDir(), "file")
		if err != nil {
			t.Fatal(err)
		}
		file := p.file
		p.file = func(fd uintptr, name string) *os.File {
			if fd == 4 {
				return notASocket
			}
			return file(fd, name)
		}

		_, err = Activated(p.env, testPid, p.file)

		assertErrorContains(t, err, "descriptor 4 (oops) from systemd: not a socket")
		if conn, err := net.Dial("unix", p.paths[0]); err == nil {
			conn.Close()
			t.Error("the socket taken before the failure is still listening")
		}
	})

	t.Run("a connection rather than a listener (Accept=yes) is refused", func(t *testing.T) {
		server, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer server.Close()
		conn, err := net.Dial("tcp4", server.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		file, err := conn.(*net.TCPConn).File()
		if err != nil {
			t.Fatal(err)
		}

		_, err = Activated(Env{"LISTEN_PID": strconv.Itoa(testPid), "LISTEN_FDS": "1"}, testPid, func(uintptr, string) *os.File { return file })

		assertErrorContains(t, err, "not a listening socket")
	})

	t.Run("a missing descriptor fails", func(t *testing.T) {
		_, err := Activated(Env{"LISTEN_PID": strconv.Itoa(testPid), "LISTEN_FDS": "1"}, testPid, func(uintptr, string) *os.File { return nil })
		assertErrorContains(t, err, "descriptor 3 (unknown) from systemd")
	})
}

func TestInheritedListenersClearsTheVariables(t *testing.T) {
	// Zero descriptors: activated in form, with none of the test process's
	// own descriptors taken.
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("LISTEN_FDS", "0")
	t.Setenv("LISTEN_FDNAMES", "")

	inherited, err := InheritedListeners()

	assertEqual(t, err, nil)
	assertEqual(t, len(inherited), 0)
	for _, name := range listenEnv {
		if _, set := os.LookupEnv(name); set {
			t.Errorf("%s is still set", name)
		}
	}
}

// listenOn is Listen over what p passes, stopped when the test ends.
func listenOn(t *testing.T, config Config, p passed) *Listening {
	t.Helper()
	inherited, err := Activated(p.env, testPid, p.file)
	if err != nil {
		t.Fatal(err)
	}
	listening, err := Listen(config, inherited, handlerSaying("app"), handlerSaying("admin"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(listening.Stop)
	return listening
}

func TestListen(t *testing.T) {
	t.Run("under socket activation, the socket named admin gets the admin handler and every other one the app", func(t *testing.T) {
		p := passing(t, passedSocket{"treesinger.socket", "unix"}, passedSocket{"admin", "unix"}, passedSocket{"treesinger.socket", "tcp"})

		listening := listenOn(t, Config{}, p)

		assertEqual(t, get(t, p.urls[0]), "app")
		assertEqual(t, get(t, p.urls[1]), "admin")
		assertEqual(t, get(t, p.urls[2]), "app")
		assertEqual(t, strings.Join(listening.URLs, " "), p.urls[0]+" admin "+p.urls[1]+" "+p.urls[2])
	})

	t.Run("under socket activation nothing else is bound", func(t *testing.T) {
		p := passing(t, passedSocket{"treesinger.socket", "unix"})
		listening := listenOn(t, Config{}, p)
		assertEqual(t, len(listening.URLs), 1)
	})

	for name, config := range map[string]Config{
		"TREESINGER_PORT": {Port: port(9090)},
		"TREESINGER_HOST": {Hostname: "127.0.0.1"},
	} {
		t.Run(name+" under socket activation is refused, and the passed sockets closed", func(t *testing.T) {
			p := passing(t, passedSocket{"treesinger.socket", "unix"})
			inherited, err := Activated(p.env, testPid, p.file)
			if err != nil {
				t.Fatal(err)
			}

			_, err = Listen(config, inherited, handlerSaying("app"), handlerSaying("admin"))

			assertErrorContains(t, err, "cannot be set under socket activation")
			if conn, err := net.Dial("unix", p.paths[0]); err == nil {
				conn.Close()
				t.Error("the passed socket is still listening")
			}
		})
	}

	t.Run("an admin descriptor that is not a unix socket is refused, and every passed socket closed", func(t *testing.T) {
		p := passing(t, passedSocket{"treesinger.socket", "unix"}, passedSocket{"admin", "tcp"})
		inherited, err := Activated(p.env, testPid, p.file)
		if err != nil {
			t.Fatal(err)
		}

		_, err = Listen(Config{}, inherited, handlerSaying("app"), handlerSaying("admin"))

		assertErrorContains(t, err, "not a unix socket")
		if conn, err := net.Dial("unix", p.paths[0]); err == nil {
			conn.Close()
			t.Error("the passed client socket is still listening")
		}
	})

	t.Run("without activation it binds TCP itself, which is never the admin handler", func(t *testing.T) {
		listening, err := Listen(Config{Port: port(0), Hostname: "127.0.0.1"}, nil, handlerSaying("app"), handlerSaying("admin"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(listening.Stop)

		assertEqual(t, len(listening.URLs), 1)
		assertEqual(t, strings.HasPrefix(listening.URLs[0], "http://127.0.0.1:"), true)
		assertEqual(t, get(t, listening.URLs[0]), "app")
	})

	t.Run("a TCP bind failure is an error", func(t *testing.T) {
		blocker, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Close()
		_, err = Listen(Config{Port: port(blocker.Addr().(*net.TCPAddr).Port), Hostname: "127.0.0.1"}, nil, handlerSaying("app"), handlerSaying("admin"))
		if err == nil {
			t.Fatal("want a bind failure")
		}
	})

	t.Run("stopping closes a passed socket but leaves its file, which is systemd's", func(t *testing.T) {
		p := passing(t, passedSocket{"treesinger.socket", "unix"})
		listening := listenOn(t, Config{}, p)

		listening.Stop()

		assertEqual(t, isSocket(p.paths[0]), true)
		if conn, err := net.Dial("unix", p.paths[0]); err == nil {
			conn.Close()
			t.Error("still listening after Stop")
		}
	})
}

func TestTheBrokerOnPassedSockets(t *testing.T) {
	h := newHarness(t, options{config: func(c *Config) { c.Port, c.Hostname = nil, "" }})
	p := passing(t, passedSocket{"treesinger.socket", "unix"}, passedSocket{"admin", "unix"})
	inherited, err := Activated(p.env, testPid, p.file)
	if err != nil {
		t.Fatal(err)
	}
	listening, err := Start(h.c, inherited, func(Env) error { return nil }, Env{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(listening.Stop)

	health := overSocket(t, p.paths[0], http.MethodGet, "/health")
	assertEqual(t, health.StatusCode, 503)
	assertJSON(t, body(t, health), `{"status":"unauthenticated"}`)
	assertEqual(t, overSocket(t, p.paths[0], http.MethodPost, "/v1/login").StatusCode, 404) // login is not the app's
	assertEqual(t, overSocket(t, p.paths[1], http.MethodGet, "/health").StatusCode, 404)    // nor health the admin's
}

func TestHealthCommand(t *testing.T) {
	t.Run("--socket probes over a unix socket", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed()})
		assertEqual(t, Health([]string{"--socket", unixServer(t, h.c.App)}, Env{}), nil)
	})

	t.Run("is unhealthy when no one has logged in", func(t *testing.T) {
		h := newHarness(t, options{})
		assertEqual(t, fmt.Sprint(Health([]string{"--socket", unixServer(t, h.c.App)}, Env{})), "unhealthy: 503 Service Unavailable")
	})

	t.Run("--url probes a base URL", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed()})
		server := httptest.NewServer(h.c.App)
		defer server.Close()
		assertEqual(t, Health([]string{"--url", server.URL + "/"}, Env{}), nil)
	})

	t.Run("by default probes TREESINGER_PORT on loopback, as the image's HEALTHCHECK", func(t *testing.T) {
		h := newHarness(t, options{seed: validSeed()})
		listening, err := Listen(Config{Port: port(0), Hostname: "127.0.0.1"}, nil, h.c.App, h.c.Admin)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(listening.Stop)
		bound := listening.listeners[0].Addr().(*net.TCPAddr).Port

		assertEqual(t, Health(nil, Env{"TREESINGER_PORT": strconv.Itoa(bound)}), nil)
	})

	t.Run("fails when nothing is listening", func(t *testing.T) {
		_, socketPath := socketIn(t)
		assertEqual(t, strings.HasPrefix(fmt.Sprint(Health([]string{"--socket", socketPath}, Env{})), "unreachable: "), true)
	})

	for name, args := range map[string][]string{
		"--socket and --url together": {"--socket", "/s", "--url", "http://x"},
		"an argument":                 {"now"},
		"an unknown option":           {"--port", "8080"},
		"an option without its value": {"--socket"},
	} {
		t.Run(name+" is a usage error", func(t *testing.T) {
			var usage *UsageError
			assertEqual(t, errors.As(Health(args, Env{}), &usage), true)
		})
	}
}

func TestDefaultHealthURL(t *testing.T) {
	tests := []struct {
		name string
		env  Env
		want string
	}{
		{"8080 when no port is set", Env{}, "http://127.0.0.1:8080"},
		{"TREESINGER_PORT", Env{"TREESINGER_PORT": "9090"}, "http://127.0.0.1:9090"},
		{"loopback for every IPv4 address", Env{"TREESINGER_HOST": "0.0.0.0"}, "http://127.0.0.1:8080"},
		{"loopback for every IPv6 address", Env{"TREESINGER_HOST": "::"}, "http://127.0.0.1:8080"},
		{"the one address it binds otherwise", Env{"TREESINGER_HOST": "10.1.2.3"}, "http://10.1.2.3:8080"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := tcpURL(test.env)
			assertEqual(t, err, nil)
			assertEqual(t, got, test.want)
		})
	}
}

func TestReadiness(t *testing.T) {
	t.Run("is signalled once, with every listener serving", func(t *testing.T) {
		p := passing(t, passedSocket{"treesinger.socket", "unix"})
		inherited, err := Activated(p.env, testPid, p.file)
		if err != nil {
			t.Fatal(err)
		}
		h := newHarness(t, options{config: func(c *Config) { c.Port, c.Hostname = nil, "" }})
		calls := 0
		notify := func(Env) error {
			calls++
			assertEqual(t, overSocket(t, p.paths[0], http.MethodGet, "/health").StatusCode, 503)
			return nil
		}

		listening, err := Start(h.c, inherited, notify, Env{"NOTIFY_SOCKET": "/run/systemd/notify"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(listening.Stop)

		assertEqual(t, calls, 1)
	})

	t.Run("a failed notify does not leave the broker listening", func(t *testing.T) {
		p := passing(t, passedSocket{"treesinger.socket", "unix"})
		inherited, err := Activated(p.env, testPid, p.file)
		if err != nil {
			t.Fatal(err)
		}
		h := newHarness(t, options{config: func(c *Config) { c.Port, c.Hostname = nil, "" }})
		failing := func(Env) error { return errors.New("no systemd") }

		_, err = Start(h.c, inherited, failing, Env{})

		assertErrorContains(t, err, "no systemd")
		if conn, err := net.Dial("unix", p.paths[0]); err == nil {
			conn.Close()
			t.Error("still listening after a failed start")
		}
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
