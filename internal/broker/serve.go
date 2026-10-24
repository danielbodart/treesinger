package broker

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/danielbodart/gokit/environ"
	"github.com/danielbodart/gokit/systemd"
)

// Listening is the listeners `serve` is serving: what to log, and how to close
// them.
type Listening struct {
	// `unix:/path` or `http://host:port` each, in the order they were passed,
	// the admin socket's marked as such.
	URLs []string

	servers   []*http.Server
	listeners []net.Listener
}

// Stop closes every listener and its connections. A socket systemd passed is
// only closed here, not unlinked: it is systemd's, and outlives this process.
func (l *Listening) Stop() {
	for i, server := range l.servers {
		server.Close()
		// Closed here too, in case Serve has not yet picked it up.
		l.listeners[i].Close()
	}
}

// idleTimeout closes a connection that sends nothing for this long, before its
// request or between requests, so idle and slow-loris clients cannot hold
// descriptors open indefinitely. http.Server has no timeouts by default, and
// is given no write timeout: a login's answer streams for as long as the
// operator takes to approve it.
const idleTimeout = 10 * time.Second

func (l *Listening) serve(listener net.Listener, handler http.Handler, label string) {
	server := &http.Server{Handler: handler, ReadHeaderTimeout: idleTimeout, IdleTimeout: idleTimeout}
	l.servers = append(l.servers, server)
	l.listeners = append(l.listeners, listener)
	l.URLs = append(l.URLs, label)
	go server.Serve(listener)
}

// AdminName is the descriptor name that marks the admin socket. Only a socket
// systemd hands over under this name is ever served the admin handler.
const AdminName = "admin"

// Listen serves app and admin. Under socket activation it serves exactly the
// sockets systemd passed (inherited): the one named "admin" gets the admin
// handler, every other one, unix or TCP, the app. The socket units then say
// where it listens, so TREESINGER_PORT/HOST saying so as well is refused
// rather than one of them silently ignored. Otherwise (Docker, development)
// it binds TCP itself, and the admin handler is not served at all: a port is
// no boundary for something that can replace the account's credentials.
//
// It returns once everything is listening. A failure closes whatever it was
// given or opened, so a half-started broker never lingers.
func Listen(config Config, inherited []systemd.Inherited, app, admin http.Handler) (*Listening, error) {
	listening := &Listening{}
	if len(inherited) > 0 {
		if config.Port != nil || config.Hostname != "" {
			closeAll(inherited)
			return nil, errors.New("TREESINGER_PORT and TREESINGER_HOST cannot be set under socket activation: the listeners come from the socket units (add a ListenStream= there instead)")
		}
		// Checked before anything is served, so a refusal serves nothing. A
		// unix socket's file mode is the admin socket's only access check; a
		// port has none, so an admin descriptor that is not a unix socket
		// (FileDescriptorName=admin on a ListenStream=PORT) is refused.
		for _, each := range inherited {
			if _, unix := each.Listener.Addr().(*net.UnixAddr); each.Name == AdminName && !unix {
				closeAll(inherited)
				return nil, fmt.Errorf("the admin descriptor is %s, not a unix socket: the admin endpoints are only ever served on a unix socket, whose mode is their access check", label(each.Listener))
			}
		}
		for _, each := range inherited {
			if each.Name == AdminName {
				listening.serve(each.Listener, admin, "admin "+label(each.Listener))
			} else {
				listening.serve(each.Listener, app, label(each.Listener))
			}
		}
		return listening, nil
	}
	listener, err := listenTCP(config)
	if err != nil {
		return nil, err
	}
	listening.serve(listener, app, label(listener))
	return listening, nil
}

func listenTCP(config Config) (net.Listener, error) {
	port := DefaultPort
	if config.Port != nil {
		port = *config.Port
	}
	host := config.Hostname
	if host == "" {
		host = "0.0.0.0"
	}
	// An IPv4 address binds IPv4 only: Go would otherwise take 0.0.0.0 to
	// mean every address, IPv6 too.
	network := "tcp"
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		network = "tcp4"
	}
	return net.Listen(network, net.JoinHostPort(host, strconv.Itoa(port)))
}

func closeAll(inherited []systemd.Inherited) {
	for _, each := range inherited {
		each.Listener.Close()
	}
}

// label is a listener as a URL a client could use.
func label(listener net.Listener) string {
	if address, ok := listener.Addr().(*net.UnixAddr); ok {
		return "unix:" + address.Name
	}
	return "http://" + listener.Addr().String()
}

// Notify is how `serve` tells a supervisor it is listening.
type Notify func(env environ.Env) error

// ready is readiness for systemd `Type=notify`: sd_notify's READY=1, sent by
// the main process itself, so it needs no helper and no NotifyAccess=all.
// Without NOTIFY_SOCKET (Docker, a shell) it does nothing.
func ready(env environ.Env) error { return systemd.Notify(env, systemd.Ready) }

// Start listens, then says so. Ready is signalled only once every listener is
// serving, so a client ordered after the service never finds it missing.
func Start(c *Container, inherited []systemd.Inherited, notify Notify, env environ.Env) (*Listening, error) {
	listening, err := Listen(*c.Config, inherited, c.App, c.Admin)
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
// closing its listeners.
func Serve(c *Container) error {
	// Cleared from the environment once read, so that nothing `serve` might
	// start mistakes systemd's variables for its own.
	inherited, err := systemd.Listeners()
	if err != nil {
		return err
	}
	listening, err := Start(c, inherited, ready, environ.Current())
	if err != nil {
		return err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	<-signals
	listening.Stop()
	return nil
}

const healthUsage = "health [--socket PATH | --url URL]"

// Health probes a running broker's `/health`: the container HEALTHCHECK,
// self-contained so the slim runtime image needs no curl/wget. Over a unix
// socket with --socket, at a base URL with --url, else the TCP port `serve`
// would bind, on loopback (or the one address it binds). Healthy only on a
// 2xx.
func Health(args []string, env environ.Env) error {
	line, err := parseOptionsOnly(healthUsage, args, "--socket", "--url")
	if err != nil {
		return err
	}
	client, base, _, err := line.broker(healthUsage, "")
	if err != nil {
		return err
	}
	if client == nil {
		client = &http.Client{}
		if base, err = tcpURL(env); err != nil {
			return err
		}
	}
	client.Timeout = 10 * time.Second
	response, err := client.Get(base + "/health")
	if err != nil {
		return fmt.Errorf("unreachable: %w", err)
	}
	response.Body.Close()
	if !succeeded(response) {
		return fmt.Errorf("unhealthy: %s", response.Status)
	}
	return nil
}

// tcpURL is the base URL of the TCP listener `serve` binds for this
// environment, on loopback when it binds every address.
func tcpURL(env environ.Env) (string, error) {
	port, host, err := tcpFromEnv(env)
	if err != nil {
		return "", err
	}
	if port == nil {
		port = new(DefaultPort)
	}
	switch host {
	case "0.0.0.0", "::", "":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(*port)), nil
}

// socketURL is the base URL of a broker reached over a unix socket. Any host
// will do; it only fills in the URL.
const socketURL = "http://treesinger"
