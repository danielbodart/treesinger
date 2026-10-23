package broker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Listening is the listeners `serve` opened: what to log, and how to close
// them.
type Listening struct {
	// `unix:/path` and/or `http://host:port`, in that order.
	URLs []string

	servers   []*http.Server
	listeners []net.Listener
}

// Stop closes every listener and its connections. Closing the unix listener
// removes its socket.
func (l *Listening) Stop() {
	for i, server := range l.servers {
		server.Close()
		// Closed here too, in case Serve has not yet picked it up.
		l.listeners[i].Close()
	}
}

// idleTimeout closes a connection that sends nothing for this long, before its
// request or between requests, so idle and slow-loris clients cannot hold
// descriptors open indefinitely. http.Server has no timeouts by default.
const idleTimeout = 10 * time.Second

func (l *Listening) serve(listener net.Listener, handler http.Handler) {
	server := &http.Server{Handler: handler, ReadHeaderTimeout: idleTimeout, IdleTimeout: idleTimeout}
	l.servers = append(l.servers, server)
	l.listeners = append(l.listeners, listener)
	go server.Serve(listener)
}

// Listen opens every configured listener (the unix socket first, then TCP)
// and returns once all of them are bound. A failure closes whatever did open,
// so a half-started broker never lingers.
func Listen(config Config, handler http.Handler) (*Listening, error) {
	listening := &Listening{}
	if err := listening.open(config, handler); err != nil {
		listening.Stop()
		return nil, err
	}
	return listening, nil
}

func (l *Listening) open(config Config, handler http.Handler) error {
	if config.SocketPath != "" {
		if err := clearStaleSocket(config.SocketPath); err != nil {
			return err
		}
		listener, err := net.Listen("unix", config.SocketPath)
		if err != nil {
			return err
		}
		l.serve(listener, handler)
		// bind() honours the umask (0755 under 022, which no one else can
		// connect() to), so the mode is set rather than left to chance. The
		// group is the process's primary group: membership is the access check.
		if err := os.Chmod(config.SocketPath, 0o660); err != nil {
			return err
		}
		l.URLs = append(l.URLs, "unix:"+config.SocketPath)
	}
	if config.Port != nil {
		// An IPv4 address binds IPv4 only: Go would otherwise take 0.0.0.0 to
		// mean every address, IPv6 too.
		network := "tcp"
		if ip := net.ParseIP(config.Hostname); ip != nil && ip.To4() != nil {
			network = "tcp4"
		}
		listener, err := net.Listen(network, net.JoinHostPort(config.Hostname, strconv.Itoa(*config.Port)))
		if err != nil {
			return err
		}
		l.serve(listener, handler)
		port := listener.Addr().(*net.TCPAddr).Port
		l.URLs = append(l.URLs, "http://"+net.JoinHostPort(config.Hostname, strconv.Itoa(port)))
	}
	return nil
}

// clearStaleSocket removes a socket left behind (a crash, a SIGKILL, or the
// directory kept across a restart by systemd's RuntimeDirectoryPreserve),
// which would make bind fail with EADDRINUSE, but only if nobody is answering
// on it. A live one is another broker, very likely on the same store, which the
// single-writer design does not allow. Anything that is not a socket is
// someone's file, and is never deleted.
func clearStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket; refusing to replace it", path)
	}
	if answering(path) {
		return fmt.Errorf("%s is already being served by another process", path)
	}
	return os.Remove(path)
}

func answering(path string) bool {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Notify is how `serve` tells a supervisor it is listening.
type Notify func(env Env) error

// SystemdNotify is readiness for systemd `Type=notify`: sd_notify's "READY=1",
// sent as one datagram to $NOTIFY_SOCKET (a path, or an abstract socket when it
// starts with '@'). Sent by the main process itself, so it needs no helper and
// no NotifyAccess=all. Without NOTIFY_SOCKET (Docker, a shell) it does
// nothing.
func SystemdNotify(env Env) error {
	path := env["NOTIFY_SOCKET"]
	if path == "" {
		return nil
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("sd_notify: %w", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("READY=1")); err != nil {
		return fmt.Errorf("sd_notify: %w", err)
	}
	return nil
}

// Start listens, then says so. Ready is signalled only once every listener is
// bound, so a client ordered after the service never finds the socket missing.
func Start(c *Container, notify Notify, env Env) (*Listening, error) {
	listening, err := Listen(*c.Config, c.App)
	if err != nil {
		return nil, err
	}
	if err := notify(env); err != nil {
		listening.Stop()
		return nil, err
	}
	c.Logger.Log(fmt.Sprintf("treesinger %s serving on %s", Version, strings.Join(listening.URLs, " and ")))
	return listening, nil
}

// Serve runs the broker. It returns only once the process is signalled, after
// closing its listeners (which removes the socket).
func Serve(c *Container) error {
	listening, err := Start(c, SystemdNotify, Environ())
	if err != nil {
		return err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	<-signals
	listening.Stop()
	return nil
}

// Health probes a running broker's `/health`: the container HEALTHCHECK,
// self-contained so the slim runtime image needs no curl/wget. Over the socket
// when one is configured, else the TCP port on loopback (or the one address it
// binds). Healthy only on a 2xx.
func Health(config Config) error {
	client := &http.Client{}
	if config.SocketPath != "" {
		client.Transport = unixTransport(config.SocketPath)
	}
	target, err := healthURL(config)
	if err != nil {
		return err
	}
	response, err := client.Get(target)
	if err != nil {
		return fmt.Errorf("unreachable: %w", err)
	}
	response.Body.Close()
	if !succeeded(response) {
		return fmt.Errorf("unhealthy: %s", response.Status)
	}
	return nil
}

func healthURL(config Config) (string, error) {
	// Any host will do over a socket; it only fills in the URL.
	if config.SocketPath != "" {
		return "http://treesinger/health", nil
	}
	if config.Port == nil {
		return "", errors.New("no listener configured")
	}
	host := config.Hostname
	switch host {
	case "0.0.0.0", "::", "":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(*config.Port)) + "/health", nil
}

// unixTransport sends every request over the unix socket at path, whatever
// the URL's host.
func unixTransport(path string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", path)
		},
	}
}
